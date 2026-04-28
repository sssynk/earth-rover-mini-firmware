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

// spawnTailscaled forks tailscaled as a fully-detached child if it's not
// already running. Same SysProcAttr.Setsid trick we use for self-daemonization.
// Idempotent: if a tailscaled is already alive on the control socket, no-op.
func spawnTailscaled(binPath, stateDir, socket, logPath string) {
	// Idempotency check: if `tailscale status` against the socket succeeds,
	// tailscaled is already up and we don't need to start it again.
	if err := exec.Command(binPath, "--socket="+socket, "status", "--peers=false").Run(); err == nil {
		log.Printf("tailscaled already running, not respawning")
		return
	}

	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("tailscaled: open log: %v", err)
		return
	}
	null, _ := os.Open(os.DevNull)
	tsdPath := "/userdata/tailscaled" // sibling of tailscale binary
	cmd := exec.Command(tsdPath,
		"--tun=userspace-networking",
		"--statedir="+stateDir,
		"--socket="+socket,
		"--port=41641",
	)
	cmd.Stdin = null
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("tailscaled: start: %v", err)
		return
	}
	log.Printf("spawned tailscaled pid %d (logs in %s)", cmd.Process.Pid, logPath)
	_ = cmd.Process.Release() // detach so we don't accumulate zombies
}

// syncTimeFromHTTP fetches the HTTP Date header from a known public server
// and sets the system clock with `date -u -s` if our current clock looks wrong
// (year < 2025). The robot has no NTP and its RTC doesn't survive boot, so
// time-sensitive workloads (Tailscale TLS, log timestamps) need this on startup.
func syncTimeFromHTTP() {
	if time.Now().Year() >= 2025 {
		return // clock looks plausible
	}
	conn, err := net.DialTimeout("tcp", "ifconfig.me:80", 5*time.Second)
	if err != nil {
		log.Printf("time sync: dial: %v", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "HEAD / HTTP/1.0\r\nHost: ifconfig.me\r\n\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.ToLower(line), "date:") {
			ts := strings.TrimSpace(line[5:])
			t, err := http.ParseTime(ts)
			if err != nil {
				log.Printf("time sync: parse: %v", err)
				return
			}
			out, err := exec.Command("date", "-u", "-s", t.UTC().Format("2006-01-02 15:04:05")).CombinedOutput()
			if err != nil {
				log.Printf("time sync: date -s: %v %s", err, out)
				return
			}
			log.Printf("time synced from ifconfig.me: %s", t.UTC().Format(time.RFC3339))
			return
		}
	}
	log.Printf("time sync: no Date header")
}

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

// UCP_STATE message values (see ucp.h's ucp_state_e enum). The STM32 uses
// these to drive the head/network status LED on its WS2812 chain:
//   0 UNKNOWN        → solid red
//   1 SIMABSENT      → red slow blink
//   2 DISCONNECTED   → green fast blink
//   3 CONNECTED      → solid green
//   4 OTA_ING        → blue fast blink
const (
	UCP_HEAD_UNKNOWN      = 0
	UCP_HEAD_SIMABSENT    = 1
	UCP_HEAD_DISCONNECTED = 2
	UCP_HEAD_CONNECTED    = 3
	UCP_HEAD_OTA          = 4
)

// StateCmd is the UCP_STATE message body (5 bytes total: 4 hd + 1 state).
type StateCmd struct {
	Len   uint16
	ID    uint8
	Index uint8
	State uint8
}

// EncodeStateFrame builds a UCP_STATE wire frame (sync 2 + struct 5 + crc 2 = 9 bytes).
func EncodeStateFrame(state, index uint8) []byte {
	m := StateCmd{Len: 5, ID: UCP_STATE, Index: index, State: state}
	var buf bytes.Buffer
	buf.Write(ucpSync)
	binary.Write(&buf, binary.LittleEndian, &m)
	crc := crc16Modbus(buf.Bytes())
	binary.Write(&buf, binary.LittleEndian, crc)
	return buf.Bytes()
}

// runNetworkStatusLoop periodically probes external internet reachability
// (TCP connect to 1.1.1.1:443 with a short timeout) and pushes the result
// to the STM32's head/network LED via UCP_STATE messages. Only writes when
// the state actually changes, so the STM32 isn't spammed.
//
// Manual POST /api/led/head calls still work — they'll be visible until the
// next auto-probe (≤ probeEvery seconds) reasserts.
func runNetworkStatusLoop(stm *os.File, probeEvery time.Duration) {
	var seq uint8
	var lastState uint8 = 255 // sentinel — guarantees first send

	push := func(s uint8, name string) {
		if s == lastState {
			return
		}
		frame := EncodeStateFrame(s, seq)
		seq++
		if _, err := stm.Write(frame); err != nil {
			log.Printf("net-led write: %v", err)
			return
		}
		log.Printf("net-led: %s", name)
		lastState = s
	}

	// Show "unknown" (red) until the first probe completes.
	push(UCP_HEAD_UNKNOWN, "probing")

	t := time.NewTicker(probeEvery)
	defer t.Stop()
	for {
		ok := false
		c, err := net.DialTimeout("tcp", "1.1.1.1:443", 3*time.Second)
		if err == nil {
			ok = true
			_ = c.Close()
		}
		if ok {
			push(UCP_HEAD_CONNECTED, "connected (1.1.1.1:443 OK)")
		} else {
			push(UCP_HEAD_DISCONNECTED, "disconnected (1.1.1.1:443 fail)")
		}
		<-t.C
	}
}

// parseHeadState accepts either a string ("connected"/"ota"/...) or a numeric
// 0..4 from JSON and returns the UCP enum value plus the canonical name.
func parseHeadState(v any) (uint8, string, bool) {
	switch x := v.(type) {
	case string:
		switch strings.ToLower(x) {
		case "unknown":
			return UCP_HEAD_UNKNOWN, "unknown", true
		case "simabsent", "no-sim", "nosim":
			return UCP_HEAD_SIMABSENT, "simabsent", true
		case "disconnected", "connecting":
			return UCP_HEAD_DISCONNECTED, "disconnected", true
		case "connected", "online", "ok":
			return UCP_HEAD_CONNECTED, "connected", true
		case "ota", "updating":
			return UCP_HEAD_OTA, "ota", true
		}
	case float64:
		n := int(x)
		if n >= 0 && n <= 4 {
			names := []string{"unknown", "simabsent", "disconnected", "connected", "ota"}
			return uint8(n), names[n], true
		}
	}
	return 0, "", false
}

// clampInt16 saturates a JSON-decoded int into the int16 range without panic.
func clampInt16(v int) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// findStatusLED returns (brightnessPath, maxBrightnessPath) for the status
// LED, trying the symlinked led-class path first and falling back to the
// platform device tree path observed in frodobot.bin's strings.
func findStatusLED() (string, string) {
	candidates := [][2]string{
		{"/sys/class/leds/status/brightness", "/sys/class/leds/status/max_brightness"},
		{"/sys/devices/platform/frodobots/gpioleds/status/brightness",
			"/sys/devices/platform/frodobots/gpioleds/status/max_brightness"},
	}
	for _, c := range candidates {
		if _, err := os.Stat(c[0]); err == nil {
			return c[0], c[1]
		}
	}
	return "", ""
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
//
// Position / quality fields come from $GxGGA. Course-over-ground and speed
// come from $GxRMC. The two NMEA sentences arrive on different lines but at
// the same 1 Hz cadence, and we merge them into one struct as they come in.
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
	// Course-over-ground (true north degrees) and speed-over-ground (m/s)
	// from $GxRMC. CourseDeg is direction of motion, NOT direction of facing
	// — going in reverse means CourseDeg is 180° off from "where the robot
	// points". Reliable when SpeedMps > ~0.3; below that the receiver may
	// emit stale or noise values.
	CourseDeg float64 `json:"course_deg"`
	SpeedMps  float64 `json:"speed_mps"`
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

// parseNMEARMC parses a $GxRMC sentence and returns (course_deg, speed_mps, ok).
// Layout: $GxRMC,hhmmss.sss,A/V,lat,N/S,lon,E/W,speed_kn,course_deg_true,date,...
// The status field 'A' = active/valid, 'V' = void; we drop 'V' frames since
// COG/speed are unreliable then. NMEA reports speed in knots, we convert to m/s.
func parseNMEARMC(line string) (float64, float64, bool) {
	if !strings.HasPrefix(line, "$") {
		return 0, 0, false
	}
	parts := strings.Split(line, ",")
	if len(parts) < 9 || !strings.HasSuffix(parts[0], "RMC") {
		return 0, 0, false
	}
	if parts[2] != "A" { // V = void / position invalid
		return 0, 0, false
	}
	speedKn, err1 := strconv.ParseFloat(parts[7], 64)
	courseDeg, err2 := strconv.ParseFloat(parts[8], 64)
	if err1 != nil && err2 != nil {
		return 0, 0, false
	}
	const knToMps = 0.514444
	return courseDeg, speedKn * knToMps, true
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
// Fan control — direct MMIO writes via the vendor's `io` tool.
// Reverse-engineered from frodobot.bin's RestApi::fanCtrl().
// -----------------------------------------------------------------------------

// setFan toggles the cooling fan on or off. Returns an error if io fails.
func setFan(on bool) error {
	var pinmux, gpio string
	if on {
		pinmux, gpio = "0x000C0004", "0x03000100"
	} else {
		pinmux, gpio = "0x000C0008", "0x03000200"
	}
	if err := exec.Command("/usr/bin/io", "-4", "0xFF5381C4", pinmux).Run(); err != nil {
		return fmt.Errorf("fan pinmux write: %w", err)
	}
	if err := exec.Command("/usr/bin/io", "-4", "0xFF5381C0", gpio).Run(); err != nil {
		return fmt.Errorf("fan gpio write: %w", err)
	}
	return nil
}

// readSoCTempC returns the SoC temperature in degrees C, or 0 on error.
func readSoCTempC() int {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return v / 1000
}

// runThermalFanLoop polls the SoC temp and toggles the fan with hysteresis.
// Reports current state to the shared `state` so the HTTP API can expose it.
func runThermalFanLoop(state *State, onAt, offAt int) {
	fanOn := false
	_ = setFan(false)
	state.setFan(false, readSoCTempC())
	for {
		time.Sleep(5 * time.Second)
		t := readSoCTempC()
		if t == 0 {
			continue
		}
		if !fanOn && t >= onAt {
			if err := setFan(true); err == nil {
				fanOn = true
				log.Printf("fan ON  at %d°C (>= %d)", t, onAt)
			}
		} else if fanOn && t <= offAt {
			if err := setFan(false); err == nil {
				fanOn = false
				log.Printf("fan OFF at %d°C (<= %d)", t, offAt)
			}
		}
		state.setFan(fanOn, t)
	}
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
	fanOn     bool
	socTempC  int
	// Headlight values, sent in every motor frame. Don't decay with the motor
	// watchdog — once you set them they stay until changed.
	frontLED int16
	backLED  int16
}

func (s *State) setLEDs(front, back int16) {
	s.mu.Lock()
	s.frontLED = front
	s.backLED = back
	s.mu.Unlock()
}

func (s *State) getLEDs() (int16, int16) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frontLED, s.backLED
}

func (s *State) setFan(on bool, tempC int) {
	s.mu.Lock()
	s.fanOn = on
	s.socTempC = tempC
	s.mu.Unlock()
}

func (s *State) getFan() (bool, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fanOn, s.socTempC
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
		st.LastError = "" // clear stale error from previous session attempt
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
	// Preserve course/speed from a recent RMC if THIS GGA has a valid fix.
	// If fix is invalid, drop them so consumers don't get stuck reading
	// stale COG/speed from minutes ago when we last had a lock.
	if g.Valid {
		g.CourseDeg = s.gps.CourseDeg
		g.SpeedMps = s.gps.SpeedMps
	}
	s.gps = g
	s.gpsOK = true
	s.mu.Unlock()
}

// setCOG updates only course-over-ground and speed (from $GxRMC). Doesn't
// touch lat/lon/altitude/fix-quality — those came from the most recent GGA.
func (s *State) setCOG(courseDeg, speedMps float64) {
	s.mu.Lock()
	s.gps.CourseDeg = courseDeg
	s.gps.SpeedMps = speedMps
	s.gps.UpdatedUnix = time.Now().Unix()
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
	fmt.Fprintln(w, "GET  /api/fan        – fan + SoC temp; POST {\"on\":bool} for manual override")
	fmt.Fprintln(w, "POST /api/imu/calibrate-mag/start  /  /end")
	fmt.Fprintln(w, "GET  /api/led/cars   – headlight values; POST {\"front\":int,\"back\":int}")
	fmt.Fprintln(w, "POST /api/led/head   – {\"state\":\"connected|disconnected|...|ota\"}")
	fmt.Fprintln(w, "GET  /api/led/status – kernel gpio-led; POST {\"value\":int}")
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
	gpsRateHz := flag.Int("gps-rate-hz", 0, "LC29H position fix rate (1..10, 0 = leave at chip default)")
	fanOnAt := flag.Int("fan-on-temp", 65, "turn cooling fan on at this SoC temp (°C)")
	fanOffAt := flag.Int("fan-off-temp", 55, "turn cooling fan off at this SoC temp (°C)")
	withTailscaled := flag.Bool("with-tailscaled", false, "spawn /userdata/tailscaled as a detached child if not already running")
	netStatusLED := flag.Bool("net-status-led", true, "auto-drive head LED based on internet reachability (TCP 1.1.1.1:443)")
	netStatusEvery := flag.Duration("net-status-interval", 5*time.Second, "how often to probe internet for the head LED")
	flag.Parse()

	if *daemon {
		daemonize(*logPath)
	}

	// Sync clock from HTTP Date header if our local time looks wrong.
	// This must happen before NTRIP/HTTPS clients try to validate certs.
	syncTimeFromHTTP()

	// Spawn tailscaled as a detached child if requested. Doing this from Go
	// (with SysProcAttr.Setsid) is reliable; doing it from the boot hook with
	// `setsid X &` was unreliable on this distro.
	if *withTailscaled {
		spawnTailscaled("/userdata/tailscale", "/userdata/tailscale-state", "/tmp/tailscaled.sock", "/tmp/tailscaled.log")
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

	// Optional: change position fix rate via $PAIR050. 200ms = 5 Hz, 100ms = 10 Hz, etc.
	if *gpsRateHz >= 1 && *gpsRateHz <= 10 {
		interval := 1000 / *gpsRateHz
		body := fmt.Sprintf("PAIR050,%d", interval)
		var cs byte
		for i := 0; i < len(body); i++ {
			cs ^= body[i]
		}
		cmd := fmt.Sprintf("$%s*%02X\r\n", body, cs)
		_, _ = gps.Write([]byte(cmd))
		log.Printf("set GPS rate to %d Hz: %s", *gpsRateHz, strings.TrimSpace(cmd))
	}

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

	// STM32 motor writer (10Hz, watchdog-driven). Also carries the headlight
	// values from setLEDs() so /api/led/cars works without a second writer.
	go func() {
		var seq uint8
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			cmd, stale := state.motorTarget(*watchdog)
			if !*allowMotor {
				continue // never write motor frames in safe mode
			}
			front, back := state.getLEDs()
			cmd.FrontLED = front
			cmd.BackLED = back
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
					} else if strings.Contains(ln, "RMC") {
						if cog, mps, ok := parseNMEARMC(ln); ok {
							state.setCOG(cog, mps)
						}
					}
					line = line[:0]
				} else if b != '\r' {
					line = append(line, b)
				}
			}
		}
	}()

	// Thermal fan control loop — keeps SoC at safe temp regardless of load.
	go runThermalFanLoop(state, *fanOnAt, *fanOffAt)

	// Auto-drive the head LED from internet reachability.
	if *netStatusLED {
		go runNetworkStatusLoop(stm, *netStatusEvery)
	}

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
	mux.HandleFunc("/api/fan", func(w http.ResponseWriter, r *http.Request) {
		on, t := state.getFan()
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, map[string]any{
				"fan_on":      on,
				"soc_temp_c":  t,
				"on_at":       *fanOnAt,
				"off_at":      *fanOffAt,
			})
		case http.MethodPost:
			var req struct {
				On *bool `json:"on"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.On == nil {
				http.Error(w, "missing 'on' field", 400)
				return
			}
			if err := setFan(*req.On); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			state.setFan(*req.On, readSoCTempC())
			writeJSON(w, 200, map[string]any{"fan_on": *req.On, "note": "auto-thermal loop will override after next tick"})
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
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

	// ----- LEDs -----------------------------------------------------------
	// Three independent outputs:
	//   /api/led/cars   → headlight & taillight (UCP MOTOR_CTL.front_led/back_led)
	//   /api/led/head   → STM32 WS2812 status LED via UCP_STATE message
	//   /api/led/status → kernel gpio-led at /sys/class/leds/status

	// Headlight & taillight values. Stored in shared state; sent in every
	// motor frame by the writer goroutine. Range/semantics aren't documented
	// in the public source — we pass through int16 verbatim and let the
	// vendor STM32 firmware interpret. Values that worked in frodobot.bin
	// (which we know it controlled live) are likely 0=off, nonzero=on, with
	// brightness possibly scaling at higher values.
	mux.HandleFunc("/api/led/cars", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f, b := state.getLEDs()
			writeJSON(w, 200, map[string]any{"front": f, "back": b})
		case http.MethodPost:
			var req struct {
				Front *int `json:"front"`
				Back  *int `json:"back"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json: "+err.Error(), 400)
				return
			}
			cf, cb := state.getLEDs()
			if req.Front != nil {
				cf = clampInt16(*req.Front)
			}
			if req.Back != nil {
				cb = clampInt16(*req.Back)
			}
			state.setLEDs(cf, cb)
			writeJSON(w, 200, map[string]any{
				"front": cf, "back": cb,
				"note": "values applied on next motor frame (~100ms)",
			})
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	})

	// Head/network status LED on the STM32 WS2812 chain.
	// Body: {"state": "unknown"|"simabsent"|"disconnected"|"connected"|"ota"} or numeric 0..4
	var headSeq uint8
	mux.HandleFunc("/api/led/head", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			State any `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), 400)
			return
		}
		s, name, ok := parseHeadState(req.State)
		if !ok {
			http.Error(w, `state must be one of: unknown|simabsent|disconnected|connected|ota (or 0..4)`, 400)
			return
		}
		frame := EncodeStateFrame(s, headSeq)
		headSeq++
		if _, err := stm.Write(frame); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{
			"sent":  fmt.Sprintf("UCP_STATE state=%d (%s)", s, name),
			"hex":   fmt.Sprintf("%x", frame),
			"note":  "STM32 firmware may or may not parse UCP_STATE — depends on shipping fw vs published source",
		})
	})

	// Kernel-class GPIO "status" LED. Auto-detect path the first time we
	// touch it (could be /sys/class/leds/status/ or the platform DT path).
	mux.HandleFunc("/api/led/status", func(w http.ResponseWriter, r *http.Request) {
		brightness, max := findStatusLED()
		if brightness == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "no status LED found at /sys/class/leds/status or /sys/devices/platform/frodobots/gpioleds/status",
			})
			return
		}
		switch r.Method {
		case http.MethodGet:
			cur, _ := os.ReadFile(brightness)
			mb, _ := os.ReadFile(max)
			writeJSON(w, 200, map[string]any{
				"value":          strings.TrimSpace(string(cur)),
				"max_brightness": strings.TrimSpace(string(mb)),
				"path":           brightness,
			})
		case http.MethodPost:
			var req struct {
				Value *int `json:"value"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Value == nil {
				http.Error(w, "expected {\"value\":int}", 400)
				return
			}
			if err := os.WriteFile(brightness, []byte(strconv.Itoa(*req.Value)), 0644); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"value": *req.Value, "path": brightness})
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
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
