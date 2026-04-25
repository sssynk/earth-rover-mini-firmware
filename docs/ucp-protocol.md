# UCP — STM32 Wire Protocol

The STM32F407 microcontroller talks to the rockchip over UART. The protocol is "UCP" (UART Control Protocol — vendor's name). It carries motor commands one way, and IMU/RPM telemetry the other way, plus a few config and OTA messages.

- **Port**: `/dev/ttyS0` on the rockchip
- **Baud**: 115200, 8N1, raw, no flow control
- **Endianness**: little-endian
- **Struct packing**: 1-byte aligned (no padding)
- **Reference C code**: `Software/Linux/src/Examples/move.cpp` in the vendor repo
- **Reference headers**: `Software/STM32/applications/ucp.h` and `Software/Linux/.../ucp.h`

## Frame format

```
┌──────┬──────┬─────┬───────┬──────────────────────┬──────┐
│ sync │ len  │ id  │ index │      body            │ CRC  │
│  2B  │  2B  │ 1B  │  1B   │     N bytes          │  2B  │
└──────┴──────┴─────┴───────┴──────────────────────┴──────┘
   │      └────────────────────────────────────────┘
   │              CRC16 (Modbus) covers
   │              [sync + len + id + index + body]
   │
   └─ 0xFD 0xFF on the wire (uint16 0xFFFD little-endian)
```

- `sync` is fixed: bytes `0xFD 0xFF` (you can write it as `*(uint16_t*)buf = 0xFFFD;` if you're on a LE machine)
- `len` is the **size of the C struct** (which already includes the 4-byte UCP header), i.e. `len = 4 + sizeof(body)`. This means a frame with body length `N` puts `N+4` in the `len` field.
- `id` selects which message struct follows (table below)
- `index` is a sequence number; STM32 increments it per outbound frame
- `body` is the typed message struct (after the 4-byte header)
- CRC16 is **Modbus-style**: init `0xFFFF`, polynomial `0xA001` (reflected `0x8005`), no final XOR. Computed over `[sync + len + id + index + body]` — i.e. **everything except the CRC itself**.

## Message types

From `ucp.h`:

| ID    | Name                  | Direction | Body size | Notes |
|-------|-----------------------|-----------|-----------|-------|
| 0x01  | KEEP_ALIVE            | both      | 0 / 1     | ping/pong; pong has 1-byte error code |
| 0x02  | MOTOR_CTL             | host→stm  | 16        | linear/angular speed, LEDs           |
| 0x03  | IMU_CORRECTION_START  | host→stm  | 1         | begin IMU/mag calibration            |
| 0x04  | IMU_CORRECTION_END    | host→stm  | 1         | end calibration (with ack body)      |
| 0x05  | RPM_REPORT            | stm→host  | 36        | telemetry, ~6 Hz, unsolicited        |
| 0x06  | IMU_WRITE             | host→stm  | 12        | write accel+gyro bias                |
| 0x07  | MAG_WRITE             | host→stm  | 6         | write magnetometer bias              |
| 0x08  | IMUMAG_READ           | host→stm  | 0/19      | request → ack with all biases        |
| 0x09  | OTA                   | host→stm  | 2/1       | firmware update request              |
| 0x0A  | STATE                 | stm→host  | varies    | network/SIM/OTA state                |

## Motor control (ID 0x02)

```c
typedef struct {
    uint16_t  len;        // = sizeof(struct) = 20
    uint8_t   id;         // = 0x02
    uint8_t   index;      // sequence
    int16_t   speed;      // -100..100 (linear)
    int16_t   angular;    // -100..100 (angular)
    int16_t   front_led;  // unimplemented in firmware — leave 0
    int16_t   back_led;   // unimplemented in firmware — leave 0
    uint16_t  version;    // 0
    uint16_t  reserve1;   // 0
    uint32_t  reserve2;   // 0
} ucp_ctl_cmd_t;          // 20 bytes
```

Wire layout for a command "go forward at speed 60":

```
fdff 1400 02 NN  3c00 0000  0000 0000  0000 0000  00000000  CRCL CRCH
└─┬─┘ └─┬─┘ ├─┘  └─┬─┘ └─┬─┘  └─┬─┘ └─┬─┘  └─┬─┘ └─┬─┘  └────┬───┘  └──┬──┘
  │     │   │     │     │      │     │       │     │         │         │
sync  len=20 id  speed angular  flf   blf    ver   res1     res2      crc16
            +index =60   =0     =0    =0     =0    =0       =0
```

24 bytes total. The vendor's example streams these at 10 Hz — see `move.cpp`.

**Watchdog behaviour**: if you stop sending motor commands, the STM32 will stop the motors after some timeout. So your control loop has to maintain a constant stream of frames (or zero-frames if you want to hold still).

## Telemetry (RPM_REPORT, ID 0x05)

```c
typedef struct {
    ucp_hd_t  hd;          // {len=40, id=0x05, index}
    uint16_t  voltage;     // battery/system voltage (units: see below)
    int16_t   rpm[4];      // wheel RPMs (signed; negative = reverse)
    int16_t   acc[3];      // MPU6050 accel x/y/z (raw LSB)
    int16_t   gyros[3];    // MPU6050 gyro x/y/z (raw LSB)
    int16_t   mag[3];      // QMC5883 magnetometer x/y/z (raw LSB)
    int16_t   heading;     // computed heading (units: see below)
    uint8_t   stop_switch; // 0..N, e-stop / "host control" status
    uint8_t   error_code;  // diagnostic code
    uint16_t  reserve;
    uint16_t  version;     // STM32 firmware version
} ucp_rep_t;               // 4-byte header + 36-byte body = 40 bytes total
```

Wire frame size: `2 (sync) + 40 + 2 (crc) = 44 bytes`.

**Observed values on a stationary, host-not-controlling robot**:
- `voltage = 100` — units unclear (could be deciVolts, percent, or raw INA226 LSB; check `Software/STM32/applications/INA226.c`)
- `rpm = [0,0,0,0]` — all stopped, as expected
- `acc = [~220, ~0, ~16900]` — accel Z ≈ 1g in some LSB scale; MPU6050 default ±2g range gives 16384 LSB/g, so 16900 LSB ≈ 1.03g — plausible
- `gyros = [~-14, 0, ~3]` — near-zero rates (stationary)
- `mag = [~1375, ~310, ~-1985]` — local magnetic field
- `heading = 102` — likely degrees? (but STM32 may use a different unit)
- `stop_switch = 3` — observed continuously, **including while we successfully drove the motors**. So it is NOT an e-stop / "host inactive" indicator. Likely a status counter or some other state field; decoding TBD by reading `Software/STM32/applications/state.c`.
- `error_code = 122` — observed at idle, drifted to 121 once motor commands started flowing. Likely also a status counter, not a fatal error.

**Test confirmed**: motors physically respond to `MOTOR_CTL` frames at 10 Hz. Sending `{speed: 8, angular: 0}` produced visible wheel rotation; non-zero `rpm[]` readings during/after motion confirm the round-trip works.

## CRC16 reference (Go)

```go
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
```

Equivalent to the lookup-table version in `move.cpp`; same result either way. Test vector: `crc16Modbus([]byte{0x01, 0x03, 0x00, 0x00})` should return `0x840A` (standard Modbus check value).

## Frame parser tips

When syncing onto the byte stream:
1. Look for byte `0xFD`. If you see it, look at the next byte.
2. If next is `0xFF`, you have sync. Read next 2 bytes as `len` (LE uint16).
3. Read `len` more bytes; the last 2 are CRC. Validate.
4. If CRC fails, drop and resync.

The `0xFD 0xFF` sequence is rare in random data, but it CAN appear inside payloads. Don't trust sync alone — always validate CRC before acting on a frame. (In practice on this protocol, false syncs are rare because the first byte after sync is always `len_lo` which constrains the value.)
