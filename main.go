package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// daemonize re-execs ourselves detached. Parent exits; child continues with
// stdin=/dev/null, stdout/stderr pointed at the given log file, and a fresh
// session id so it survives the originating shell.
func daemonize(logPath string) {
	if os.Getenv("DAEMONIZED") == "1" {
		return // already the child
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemonize: open log: %v\n", err)
		os.Exit(1)
	}
	null, _ := os.Open(os.DevNull)
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), "DAEMONIZED=1")
	cmd.Stdin = null
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "daemonize: start: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "daemonized as pid %d, logging to %s\n", cmd.Process.Pid, logPath)
	os.Exit(0)
}

// -----------------------------------------------------------------------------
// UCP wire protocol (STM32 link on /dev/ttyS0)
// -----------------------------------------------------------------------------

const (
	UCP_KEEP_ALIVE           = 0x01
	UCP_MOTOR_CTL            = 0x02
	UCP_IMU_CORRECTION_START = 0x03
	UCP_IMU_CORRECTION_END   = 0x04
	UCP_RPM_REPORT           = 0x05
	UCP_STATE                = 0x0A

	UICT_MAG = 1
	UICT_IMU = 2
)

// IMUCorrect matches ucp_imu_correct_t (5 bytes packed LE).
type IMUCorrect struct {
	Len   uint16
	ID    uint8
	Index uint8
	Type  uint8
}

// EncodeIMUCorrect builds a sync+struct+crc frame for START or END calibration.
func EncodeIMUCorrect(end bool, calType, index uint8) []byte {
	id := uint8(UCP_IMU_CORRECTION_START)
	if end {
		id = UCP_IMU_CORRECTION_END
	}
	m := IMUCorrect{Len: 5, ID: id, Index: index, Type: calType}
	var buf bytes.Buffer
	buf.Write(ucpSync)
	binary.Write(&buf, binary.LittleEndian, &m)
	crc := crc16Modbus(buf.Bytes())
	binary.Write(&buf, binary.LittleEndian, crc)
	return buf.Bytes()
}

var ucpSync = []byte{0xfd, 0xff}

// crc16Modbus: init 0xFFFF, poly 0xA001 (matches move.cpp lookup tables).
func crc16Modbus(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// MotorCmd matches ucp_ctl_cmd_t (20 bytes packed LE).
type MotorCmd struct {
	Len      uint16
	ID       uint8
	Index    uint8
	Speed    int16
	Angular  int16
	FrontLED int16
	BackLED  int16
	Version  uint16
	Reserve1 uint16
	Reserve2 uint32
}

// EncodeFrame: sync(2) + struct(20) + crc(2) = 24 bytes.
func (m MotorCmd) EncodeFrame() []byte {
	m.Len = 20
	m.ID = UCP_MOTOR_CTL
	var buf bytes.Buffer
	buf.Write(ucpSync)
	binary.Write(&buf, binary.LittleEndian, &m)
	crc := crc16Modbus(buf.Bytes())
	binary.Write(&buf, binary.LittleEndian, crc)
	return buf.Bytes()
}

// RpmReport matches ucp_rep_t body (36 bytes after the 4-byte ucp_hd_t).
type RpmReport struct {
	Voltage    uint16
	Rpm        [4]int16
	Acc        [3]int16
	Gyros      [3]int16
	Mag        [3]int16
	Heading    int16
	StopSwitch uint8
	ErrorCode  uint8
	Reserve    uint16
	Version    uint16
}

// readUCPFrames consumes a stream, syncs on 0xfd 0xff, validates Modbus CRC16,
// invokes handler(id, index, body) per valid frame.
func readUCPFrames(r io.Reader, handler func(id, index uint8, body []byte)) error {
	buf := make([]byte, 4096)
	state := 0
	var pkt []byte
	var pktLen int
	for {
		n, err := r.Read(buf)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			b := buf[i]
			switch state {
			case 0:
				if b == 0xfd {
					state = 1
				}
			case 1:
				if b == 0xff {
					state = 2
					pkt = pkt[:0]
					pkt = append(pkt, 0xfd, 0xff)
				} else if b != 0xfd {
					state = 0
				}
			case 2:
				pkt = append(pkt, b)
				if len(pkt) == 4 {
					pktLen = int(binary.LittleEndian.Uint16(pkt[2:4]))
					if pktLen < 4 || pktLen > 1024 {
						state = 0
						continue
					}
					state = 3
				}
			case 3:
				pkt = append(pkt, b)
				if len(pkt) == 2+pktLen+2 {
					end := 2 + pktLen
					gotCRC := binary.LittleEndian.Uint16(pkt[end : end+2])
					wantCRC := crc16Modbus(pkt[:end])
					if gotCRC == wantCRC {
						handler(pkt[4], pkt[5], append([]byte(nil), pkt[6:end]...))
					}
					state = 0
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// NMEA (LC29H GNSS link on /dev/ttyS2)
// -----------------------------------------------------------------------------

// GPSFix is the parsed view of the latest $GxGGA + $GxRMC.
type GPSFix struct {
	Time        string  `json:"time_utc"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	AltM        float64 `json:"altitude_m"`
	FixQuality  int     `json:"fix_quality"`
	NumSats     int     `json:"num_sats"`
	HDOP        float64 `json:"hdop"`
	Valid       bool    `json:"valid"`
	UpdatedUnix int64   `json:"updated_unix"`
}

// parseNMEAGGA parses a $GxGGA sentence into a GPSFix (partially).
func parseNMEAGGA(line string) (GPSFix, bool) {
	// $xxGGA,time,lat,N/S,lon,E/W,fixQ,nsat,hdop,alt,M,...
	if !strings.HasPrefix(line, "$") {
		return GPSFix{}, false
	}
	parts := strings.Split(line, ",")
	if len(parts) < 10 || !strings.HasSuffix(parts[0], "GGA") {
		return GPSFix{}, false
	}
	fix := GPSFix{Time: parts[1]}
	if parts[2] != "" && parts[3] != "" {
		fix.Lat = nmeaToDeg(parts[2], parts[3])
	}
	if parts[4] != "" && parts[5] != "" {
		fix.Lon = nmeaToDeg(parts[4], parts[5])
	}
	fix.FixQuality, _ = strconv.Atoi(parts[6])
	fix.NumSats, _ = strconv.Atoi(parts[7])
	fix.HDOP, _ = strconv.ParseFloat(parts[8], 64)
	fix.AltM, _ = strconv.ParseFloat(parts[9], 64)
	fix.Valid = fix.FixQuality > 0
	return fix, true
}

// nmeaToDeg converts "DDMM.MMMM" + "N/S/E/W" to signed decimal degrees.
func nmeaToDeg(coord, hemi string) float64 {
	if len(coord) < 4 {
		return 0
	}
	dot := strings.IndexByte(coord, '.')
	if dot < 2 {
		return 0
	}
	degDigits := dot - 2
	deg, _ := strconv.ParseFloat(coord[:degDigits], 64)
	min, _ := strconv.ParseFloat(coord[degDigits:], 64)
	v := deg + min/60.0
	if hemi == "S" || hemi == "W" {
		v = -v
	}
	return v
}

// -----------------------------------------------------------------------------
// Shared state
// -----------------------------------------------------------------------------

type Telemetry struct {
	RpmReport
	UpdatedUnix int64 `json:"updated_unix"`
	Index       uint8 `json:"index"`
}

type State struct {
	mu        sync.RWMutex
	tel       Telemetry
	telOK     bool
	gps       GPSFix
	gpsOK     bool
	lastGGA   string // raw NMEA line, sent verbatim to NTRIP caster
	curMotor  MotorCmd
	lastMotor time.Time
	ntrip     NTRIPStatus
}

func (s *State) setGGA(line string) {
	s.mu.Lock()
	s.lastGGA = line
	s.mu.Unlock()
}

func (s *State) getGGA() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastGGA
}

func (s *State) setNTRIP(n NTRIPStatus) {
	s.mu.Lock()
	s.ntrip = n
	s.mu.Unlock()
}

func (s *State) getNTRIP() NTRIPStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ntrip
}

// NTRIPStatus is exposed via /api/rtk/status.
type NTRIPStatus struct {
	Enabled     bool   `json:"enabled"`
	Connected   bool   `json:"connected"`
	Host        string `json:"host"`
	Mount       string `json:"mount"`
	BytesIn     int64  `json:"bytes_in"`
	LastError   string `json:"last_error,omitempty"`
	LastGGASent string `json:"last_gga_sent,omitempty"`
	UpdatedUnix int64  `json:"updated_unix"`
}

// NTRIPConfig is filled from CLI flags.
type NTRIPConfig struct {
	Host  string
	Port  int
	Mount string
	User  string
	Pass  string
}

// runNTRIP loops forever: connect, stream RTCM into `out`, reconnect on errors.
// `getGGA` returns the most recent valid $GxGGA from the chip (or empty until one arrives).
func runNTRIP(cfg NTRIPConfig, getGGA func() string, out io.Writer, status func(NTRIPStatus)) {
	st := NTRIPStatus{Enabled: true, Host: cfg.Host, Mount: cfg.Mount}
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.User + ":" + cfg.Pass))
	for {
		st.Connected = false
		st.UpdatedUnix = time.Now().Unix()
		status(st)

		// Wait for at least one real GGA before connecting — caster routes "AUTO" by it.
		var firstGGA string
		for {
			firstGGA = getGGA()
			if firstGGA != "" {
				break
			}
			time.Sleep(1 * time.Second)
		}

		err := ntripSession(cfg, auth, getGGA, out, &st, status)
		st.LastError = ""
		if err != nil {
			st.LastError = err.Error()
		}
		st.Connected = false
		st.UpdatedUnix = time.Now().Unix()
		status(st)
		log.Printf("ntrip session ended: %v; reconnecting in 5s", err)
		time.Sleep(5 * time.Second)
	}
}

func ntripSession(cfg NTRIPConfig, auth string, getGGA func() string,
	out io.Writer, st *NTRIPStatus, status func(NTRIPStatus)) error {
	conn, err := net.DialTimeout("tcp",
		fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	req := fmt.Sprintf(
		"GET /%s HTTP/1.0\r\n"+
			"User-Agent: frodobot-custom/1.0\r\n"+
			"Ntrip-Version: Ntrip/2.0\r\n"+
			"Authorization: Basic %s\r\n\r\n",
		cfg.Mount, auth)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	statusLine = strings.TrimSpace(statusLine)
	if !strings.Contains(statusLine, "200") {
		return fmt.Errorf("caster rejected: %s", statusLine)
	}
	// Drain headers up to blank line (Ntrip 2.0 / HTTP). For ICY responses,
	// there are no headers — the next bytes are RTCM directly.
	if !strings.HasPrefix(statusLine, "ICY") {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read header: %w", err)
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
	}

	st.Connected = true
	st.UpdatedUnix = time.Now().Unix()
	status(*st)

	// Send initial GGA + start periodic GGA sender.
	if g := getGGA(); g != "" {
		_, _ = conn.Write([]byte(g + "\r\n"))
		st.LastGGASent = g
	}
	stopGGA := make(chan struct{})
	defer close(stopGGA)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopGGA:
				return
			case <-t.C:
				if g := getGGA(); g != "" {
					if _, err := conn.Write([]byte(g + "\r\n")); err != nil {
						return
					}
					st.LastGGASent = g
				}
			}
		}
	}()

	// Forward RTCM to out, refreshing read deadline as bytes arrive.
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := br.Read(buf)
		if err != nil {
			return fmt.Errorf("read rtcm: %w", err)
		}
		if _, err := out.Write(buf[:n]); err != nil {
			return fmt.Errorf("write to gps: %w", err)
		}
		st.BytesIn += int64(n)
		st.UpdatedUnix = time.Now().Unix()
		status(*st)
	}
}

func (s *State) setTelemetry(idx uint8, r RpmReport) {
	s.mu.Lock()
	s.tel = Telemetry{RpmReport: r, UpdatedUnix: time.Now().Unix(), Index: idx}
	s.telOK = true
	s.mu.Unlock()
}

func (s *State) getTelemetry() (Telemetry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tel, s.telOK
}

func (s *State) setGPS(g GPSFix) {
	g.UpdatedUnix = time.Now().Unix()
	s.mu.Lock()
	s.gps = g
	s.gpsOK = true
	s.mu.Unlock()
}

func (s *State) getGPS() (GPSFix, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gps, s.gpsOK
}

// setMotor records a new commanded motor target. Watchdog reads this each tick.
func (s *State) setMotor(m MotorCmd) {
	s.mu.Lock()
	s.curMotor = m
	s.lastMotor = time.Now()
	s.mu.Unlock()
}

// motorTarget returns (cmd, isStale) based on watchdog timeout.
func (s *State) motorTarget(timeout time.Duration) (MotorCmd, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stale := time.Since(s.lastMotor) > timeout
	if stale {
		return MotorCmd{}, true // zero command if stale
	}
	return s.curMotor, false
}

// -----------------------------------------------------------------------------
// HTTP handlers
// -----------------------------------------------------------------------------

type StreamInfo struct {
	URL        string `json:"url"`
	Sensor     string `json:"sensor"`
	Resolution string `json:"resolution"`
	Codec      string `json:"codec"`
}

func defaultStreams(host string) []StreamInfo {
	return []StreamInfo{
		{fmt.Sprintf("rtsp://%s:554/live/0", host), "front-main", "1920x1080", "H265"},
		{fmt.Sprintf("rtsp://%s:554/live/1", host), "front-sub", "720x576", "H265"},
		{fmt.Sprintf("rtsp://%s:554/live/2", host), "back-main", "1920x1080", "H265"},
		{fmt.Sprintf("rtsp://%s:554/live/3", host), "back-sub", "720x576", "H265"},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "frodobot custom firmware")
	fmt.Fprintln(w, "GET  /api/telemetry  – latest STM32 telemetry")
	fmt.Fprintln(w, "GET  /api/gps        – latest GPS fix")
	fmt.Fprintln(w, "GET  /api/streams    – RTSP URLs")
	fmt.Fprintln(w, "POST /api/motor      – {speed:int, angular:int} (-100..100)")
	fmt.Fprintln(w, "GET  /api/rtk/status – NTRIP/RTK status")
	fmt.Fprintln(w, "POST /api/imu/calibrate-mag/start  /  /end")
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func openSerial(dev string, baud int) (*os.File, error) {
	if err := exec.Command("stty", "-F", dev, strconv.Itoa(baud), "raw", "-echo", "-hupcl").Run(); err != nil {
		return nil, fmt.Errorf("stty %s: %w", dev, err)
	}
	return os.OpenFile(dev, os.O_RDWR, 0)
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	allowMotor := flag.Bool("allow-motor", false, "accept POST /api/motor — robot can move")
	maxSpeed := flag.Int("max-speed", 60, "absolute cap on speed/angular magnitude (1..100)")
	stmDev := flag.String("stm32", "/dev/ttyS0", "STM32 serial device")
	gpsDev := flag.String("gps", "/dev/ttyS2", "LC29H GNSS serial device")
	watchdog := flag.Duration("motor-watchdog", 500*time.Millisecond, "auto-stop motor if no command in this window")
	daemon := flag.Bool("daemon", false, "fork into background, redirect output to -log")
	logPath := flag.String("log", "/tmp/robot.log", "log file path when -daemon")
	ntripHost := flag.String("ntrip-host", "", "NTRIP caster host (e.g. rtk.geodnet.com)")
	ntripPort := flag.Int("ntrip-port", 2101, "NTRIP caster port")
	ntripMount := flag.String("ntrip-mount", "AUTO", "NTRIP mountpoint")
	ntripUser := flag.String("ntrip-user", "", "NTRIP username")
	ntripPass := flag.String("ntrip-pass", "", "NTRIP password")
	flag.Parse()

	if *daemon {
		daemonize(*logPath)
	}

	state := &State{}
	state.lastMotor = time.Unix(0, 0) // ensure stale at startup

	// STM32 link
	stm, err := openSerial(*stmDev, 115200)
	if err != nil {
		log.Fatalf("STM32 open: %v", err)
	}
	defer stm.Close()

	// GPS link
	gps, err := openSerial(*gpsDev, 115200)
	if err != nil {
		log.Fatalf("GPS open: %v", err)
	}
	defer gps.Close()

	// Wake GNSS in case it's stopped
	_, _ = gps.Write([]byte("$PQTMGNSSSTART*51\r\n"))

	// STM32 reader
	go func() {
		err := readUCPFrames(stm, func(id, index uint8, body []byte) {
			if id != UCP_RPM_REPORT || len(body) < 36 {
				return
			}
			var rep RpmReport
			_ = binary.Read(bytes.NewReader(body), binary.LittleEndian, &rep)
			state.setTelemetry(index, rep)
		})
		log.Printf("STM32 reader exited: %v", err)
	}()

	// STM32 motor writer (10Hz, watchdog-driven)
	go func() {
		var seq uint8
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			cmd, stale := state.motorTarget(*watchdog)
			if !*allowMotor {
				continue // never write motor frames in safe mode
			}
			cmd.Index = seq
			seq++
			if _, err := stm.Write(cmd.EncodeFrame()); err != nil {
				log.Printf("motor write: %v", err)
				return
			}
			_ = stale
		}
	}()

	// GPS reader: parses NMEA, also stashes raw GGA for upstream NTRIP.
	go func() {
		buf := make([]byte, 4096)
		var line []byte
		for {
			n, err := gps.Read(buf)
			if err != nil {
				log.Printf("GPS read: %v", err)
				return
			}
			for i := 0; i < n; i++ {
				b := buf[i]
				if b == '\n' {
					ln := strings.TrimSpace(string(line))
					if strings.Contains(ln, "GGA") {
						state.setGGA(ln)
						if fix, ok := parseNMEAGGA(ln); ok {
							state.setGPS(fix)
						}
					}
					line = line[:0]
				} else if b != '\r' {
					line = append(line, b)
				}
			}
		}
	}()

	// NTRIP client: only runs if creds are provided.
	if *ntripHost != "" && *ntripUser != "" && *ntripPass != "" {
		log.Printf("NTRIP enabled: %s:%d /%s as %s", *ntripHost, *ntripPort, *ntripMount, *ntripUser)
		go runNTRIP(
			NTRIPConfig{Host: *ntripHost, Port: *ntripPort, Mount: *ntripMount, User: *ntripUser, Pass: *ntripPass},
			state.getGGA,
			gps, // RTCM bytes go straight to the same fd we're reading NMEA from
			state.setNTRIP,
		)
	}

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/telemetry", func(w http.ResponseWriter, r *http.Request) {
		t, ok := state.getTelemetry()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no telemetry yet"})
			return
		}
		writeJSON(w, 200, t)
	})
	mux.HandleFunc("/api/gps", func(w http.ResponseWriter, r *http.Request) {
		g, ok := state.getGPS()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no gps yet"})
			return
		}
		writeJSON(w, 200, g)
	})
	mux.HandleFunc("/api/rtk/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, state.getNTRIP())
	})
	// Magnetometer calibration. Caller spins the robot ~360° between start and end.
	var calSeq uint8
	mux.HandleFunc("/api/imu/calibrate-mag/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		frame := EncodeIMUCorrect(false, UICT_MAG, calSeq)
		calSeq++
		if _, err := stm.Write(frame); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{
			"sent":     "UCP_IMU_CORRECTION_START type=MAG",
			"hex":      fmt.Sprintf("%x", frame),
			"next":     "spin the robot ~360° in place, then POST /api/imu/calibrate-mag/end",
		})
	})
	mux.HandleFunc("/api/imu/calibrate-mag/end", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		frame := EncodeIMUCorrect(true, UICT_MAG, calSeq)
		calSeq++
		if _, err := stm.Write(frame); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{
			"sent": "UCP_IMU_CORRECTION_END type=MAG",
			"hex":  fmt.Sprintf("%x", frame),
			"next": "watch /api/telemetry — Heading and Mag values should be re-zeroed",
		})
	})
	mux.HandleFunc("/api/streams", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := strings.Cut(r.Host, ":")
		if host == "" {
			host = "192.168.11.1"
		}
		writeJSON(w, 200, defaultStreams(host))
	})
	mux.HandleFunc("/api/motor", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !*allowMotor {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "motor disabled — start with -allow-motor",
			})
			return
		}
		var req struct {
			Speed   int `json:"speed"`
			Angular int `json:"angular"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Clamp to safety bounds
		clamp := func(v, lim int) int {
			if v > lim {
				return lim
			}
			if v < -lim {
				return -lim
			}
			return v
		}
		req.Speed = clamp(req.Speed, *maxSpeed)
		req.Angular = clamp(req.Angular, *maxSpeed)
		state.setMotor(MotorCmd{Speed: int16(req.Speed), Angular: int16(req.Angular)})
		writeJSON(w, 200, map[string]any{
			"accepted": req,
			"watchdog": watchdog.String(),
			"note":     "send another command within watchdog window or motor will stop",
		})
	})

	log.Printf("listening on %s (allow-motor=%v, max-speed=%d, watchdog=%v)",
		*listen, *allowMotor, *maxSpeed, *watchdog)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
