/*
TinyTrek RIGHT stepper motor node (CAN id 0x113).
Microcontroller: Adafruit QT Py SAMD21 + MCP2515 CAN (CS=3, INT=0).
Identical to TinytrekLMotor except for canId -- keep the two in sync.
*/

#include <CAN.h>
#include <Adafruit_NeoPixel.h>

const int dirPin = 4;
const int stepPin = 5;
const int enablePin = 6;

int FORWARD = HIGH;
int BACKWARD = LOW;

// Steps for one revolution on stepper motor
int stepsPerRev = 200;

// Desired speed
int rpm = 100;

int canId = 0x113;

// --- Consumed step buffer (tolerates dropped CAN frames) --------------------
// Each command ADDS steps to a buffer (capped) and sets speed/direction; the
// motor drains the buffer as it steps, so motion follows the BUFFER, not message
// timing. A dropped frame or two doesn't stall it while the buffer holds steps --
// that smooths over intermittent CAN loss. The cap bounds how far it coasts and
// makes runaway impossible: if commands stop, it empties and halts within
// STEPS_MAX steps. dir 0x00, or a change of direction, clears the buffer.
const long STEPS_MAX = 250;   // ~2.5x the dashboard's per-frame chunk -> jitter cushion

long          stepsLeft  = 0;      // steps still to run (the consumed buffer)
int           driveDir   = FORWARD;
int           driveRpm   = 100;
unsigned long lastStepUs = 0;
bool          driverOn   = false;

// --- System-state inputs (for the status LED) -------------------------------
// The node watches two periodic messages so the LED reports the health of the
// WHOLE chain, not just this board -- which is what makes it a wiring-diagnosis
// tool: whatever is missing tells you which link is broken.
//   0x100 heartbeat from the Pi  : [seq][flags]        -- "the Pi is alive + in control"
//   0x116 beacon from the BMS    : [v_hi][v_lo][pwr][flags] -- "12V rail is really on"
// pwr: 0x01 = 12V on, 0x02 = off (same encoding as the 0x115 command).
// ---- node provisioning: pushed ONCE, then stored ---------------------------
// The Pi sends this node its Data ID during PROVISIONING -- an operator action,
// performed before the event -- and the node writes it to flash. At runtime it is
// read from flash and NOTHING IS TRANSMITTED, so the value that Challenge 3 exists
// to recover is never on the bus while a contestant is on it.
//
// A configured node IGNORES further config frames. Otherwise anyone who reached the
// drive bus could simply overwrite the Data ID with one they already knew and forge
// freely, which would turn Challenge 3 into "get relay access". Re-provisioning a
// node means erasing it deliberately (hold-to-erase at boot, below) or reflashing.
//
// UNCONFIGURED IS PERMISSIVE: a node with nothing stored accepts the unprotected
// 6-byte command, because an unprovisioned car must still drive. That is fail-open,
// and it is covered -- the Pi's bridge and relay, the only two routes a contestant
// has onto this bus, validate protection independently.
#include <FlashAsEEPROM.h>

const long NODE_CFG_ID = 0x101;
const uint8_t CFG_MAGIC = 0xC7;   // marks a written record; anything else = empty

bool     haveDataId = false;
uint16_t myDataId   = 0;

static void cfgLoad() {
  if (!EEPROM.isValid()) return;
  if (EEPROM.read(0) != CFG_MAGIC) return;
  myDataId   = ((uint16_t)EEPROM.read(1) << 8) | EEPROM.read(2);
  haveDataId = true;
}

static void cfgStore(uint16_t id) {
  EEPROM.write(0, CFG_MAGIC);
  EEPROM.write(1, (uint8_t)(id >> 8));
  EEPROM.write(2, (uint8_t)(id & 0xFF));
  EEPROM.commit();               // one flash-page write; provisioning only
  myDataId   = id;
  haveDataId = true;
}

// CRC-8/SAE-J1850 (poly 0x1D, init 0xFF, xorout 0xFF) keyed by a 16-bit Data ID,
// AUTOSAR E2E Profile 1 style: Data ID prefixed LOW BYTE FIRST, then the covered
// bytes. Must match protectionCRC() in cmd/dashboard/protect.go byte for byte.
// Check value: over "123456789" with no key, 0x4B.
static uint8_t crc8J1850Keyed(const uint8_t* data, uint8_t len, uint16_t dataId) {
  uint8_t crc = 0xFF;
  uint8_t pre[2] = { (uint8_t)(dataId & 0xFF), (uint8_t)(dataId >> 8) };
  for (uint8_t i = 0; i < 2; i++) {
    crc ^= pre[i];
    for (uint8_t b = 0; b < 8; b++) crc = (crc & 0x80) ? (uint8_t)((crc << 1) ^ 0x1D) : (uint8_t)(crc << 1);
  }
  for (uint8_t i = 0; i < len; i++) {
    crc ^= data[i];
    for (uint8_t b = 0; b < 8; b++) crc = (crc & 0x80) ? (uint8_t)((crc << 1) ^ 0x1D) : (uint8_t)(crc << 1);
  }
  return crc ^ 0xFF;
}

const long HEARTBEAT_ID  = 0x100;
const long BMS_BEACON_ID = 0x116;
// Both are sent at ~5 Hz, so 1 s is 5 missed messages -- slow enough not to
// flicker on a busy bus, fast enough to see a cable pulled.
const unsigned long HEARTBEAT_TIMEOUT_MS = 1000;
const unsigned long BEACON_TIMEOUT_MS    = 1000;

unsigned long lastHeartbeatMs = 0;
unsigned long lastBeaconMs    = 0;
bool          haveHeartbeat   = false;   // distinct from "millis()==0 at boot"
bool          haveBeacon      = false;
bool          beacon12v       = false;   // last reported 12V rail state

// --- CAN bring-up -----------------------------------------------------------
// The MCP2515 needs its oscillator running before it will answer over SPI: 128
// oscillator cycles for the start-up timer PLUS crystal settling, which from a
// cold power-on is hundreds of microseconds to milliseconds. The library's
// reset() waits only 10us, so begin()'s CANCTRL readback can lose that race at
// power-on and succeed on a warm reset.
//
// A board that loses it used to stay deaf FOREVER: begin() failed, the message was
// printed to a serial port nobody was attached to, and setup() carried on. The
// controller sits in configuration mode -- electrically passive, so it neither
// acknowledges frames nor corrupts the bus. From the Pi the bus looks perfectly
// healthy (another node ACKs) while this node hears nothing. Diagnosed exactly
// once, the hard way, on 2026-08-02.
//
// So: settle before the first attempt, and RETRY until it works.
const long CAN_BITRATE = 500000;

// MCP2515 module crystal. begin() does NOT verify this -- it picks bit timing from
// whatever we claim here, so an 8 MHz module told 16 MHz reports success and then
// runs at half rate (deaf, and it corrupts the bus with error frames). Override at
// build time for 8 MHz modules: --build-property "build.extra_flags=-DCAN_CLOCK_HZ=8000000"
#ifndef CAN_CLOCK_HZ
#define CAN_CLOCK_HZ 16000000L
#endif

// 10 MHz (the library default) is the MCP2515's absolute maximum and is marginal
// over hand-wired jumpers. 4 MHz costs nothing at this frame rate and is far more
// tolerant of a mediocre connection.
const uint32_t CAN_SPI_HZ    = 4000000;
const unsigned long CAN_SETTLE_MS = 50;    // let the crystal stabilise before try #1
const unsigned long CAN_RETRY_MS  = 1000;  // re-attempt cadence while down
const unsigned long RX_GRACE_MS   = 3000;  // quiet period before "never received"

bool          canUp        = false;
bool          everRx       = false;   // has ANY frame ever arrived?
unsigned long lastCanTryMs = 0;
unsigned long canUpMs      = 0;
unsigned long canTries     = 0;

// canTryBegin re-applies every setting, because a retry runs after a failed
// begin() left the controller in an unknown state.
bool canTryBegin() {
  canTries++;
  CAN.setPins(3, 0);
  CAN.setSPIFrequency(CAN_SPI_HZ);
  CAN.setClockFrequency(CAN_CLOCK_HZ);
  return CAN.begin(CAN_BITRATE) == 1;
}

// --- Status NeoPixel (onboard; QT Py SAMD21 = pin 11, no power-gate pin) -----
//   BLUE   solid  : CAN CONTROLLER DID NOT INITIALISE -- SPI/board fault, retrying
//   BLUE   blink  : controller up, but NOT ONE frame ever received -- bus/wiring
//   GREEN  solid  : READY  -- heartbeat + 12V both good, not moving
//   YELLOW solid  : ACTIVE -- propulsion commanded and running
//   RED    solid  : no heartbeat AND no 12V   (both missing)
//   RED    double : no heartbeat from the Pi  (12V is fine)
//   RED    blink  : no 12V active             (heartbeat is fine)
//
// The two blue states are the important addition: they used to be indistinguishable
// from solid red, so "this board is broken" and "this bus is disconnected" looked
// identical on the robot.
#ifdef PIN_NEOPIXEL
Adafruit_NeoPixel pixel(1, PIN_NEOPIXEL, NEO_GRB + NEO_KHZ800);
#else
Adafruit_NeoPixel pixel(1, 11, NEO_GRB + NEO_KHZ800);
#endif
const uint8_t LED_BRIGHTNESS = 64;   // 0..255; bright enough to read, not blinding
const unsigned long LED_UPDATE_MS = 20;

unsigned long lastLedMs = 0;
uint32_t      ledLast   = 0xFFFFFFFF;   // force the first show()

const unsigned long STATUS_MS = 2000;   // serial status cadence
unsigned long lastStatusMs = 0;
void reportStatus();

bool heartbeatOK() {
  return haveHeartbeat && (millis() - lastHeartbeatMs) < HEARTBEAT_TIMEOUT_MS;
}

// powerOK is true only on a FRESH beacon that says the rail is on. A missing
// beacon counts as "no 12V" -- an unwired/dead BMS should not read as ready.
bool powerOK() {
  return haveBeacon && (millis() - lastBeaconMs) < BEACON_TIMEOUT_MS && beacon12v;
}

// updateLed is non-blocking (throttled, and show() only on a change) so it never
// disturbs step timing.
void updateLed() {
  if (millis() - lastLedMs < LED_UPDATE_MS) return;
  lastLedMs = millis();

  uint8_t r, g, b;
  bool on = true;

  if (!canUp) {
    // BLUE solid: the controller itself never came up. A BOARD fault (SPI wiring,
    // dead MCP2515), not a bus fault -- no amount of cabling work will fix it.
    r = 0; g = 0; b = 255;
  } else if (!everRx && (millis() - canUpMs) > RX_GRACE_MS) {
    // BLUE blink: controller is alive and configured but has never heard a single
    // frame. A healthy bus always carries the BMS beacon at 5 Hz, so this is
    // wiring, termination, or a bit-rate mismatch -- the node is deaf, not broken.
    r = 0; g = 0; b = 255;
    on = (millis() % 900) < 450;
  } else {
    bool hb  = heartbeatOK();
    bool pwr = powerOK();
    if (hb && pwr) {
      if (stepsLeft > 0) { r = 255; g = 180; b = 0; } // YELLOW: propulsion active
      else               { r = 0;   g = 255; b = 0; } // GREEN: ready, idle
    } else {
      r = 255; g = 0; b = 0;                          // RED: not ready
      if (!hb && !pwr) {
        on = true;                                    // solid: both missing
      } else if (!hb) {
        unsigned long t = millis() % 1200;            // double blink: no Pi heartbeat
        on = (t < 130) || (t >= 260 && t < 390);
      } else {
        on = (millis() % 900) < 450;                  // regular blink: no 12V
      }
    }
  }

  uint32_t c = on ? pixel.Color(r, g, b) : 0;
  if (c != ledLast) {
    pixel.setPixelColor(0, c);
    pixel.show();
    ledLast = c;
  }
}

void setup() {
  // Read the provisioned Data ID from flash. Nothing is transmitted at runtime.
  cfgLoad();
  pinMode(dirPin, OUTPUT);
  pinMode(stepPin, OUTPUT);
  pinMode(enablePin, OUTPUT);

  // Status LED first, so a board that fails CAN init still shows solid red
  // (which is exactly the "nothing is reaching me" state we want to see).
#ifdef NEOPIXEL_POWER
  pinMode(NEOPIXEL_POWER, OUTPUT);
  digitalWrite(NEOPIXEL_POWER, HIGH);
#endif
  pixel.begin();
  pixel.setBrightness(LED_BRIGHTNESS);
  pixel.setPixelColor(0, pixel.Color(255, 0, 0));
  pixel.show();

  Serial.begin(115200);   // no while(!Serial): a car is never attached to a laptop

  // Disable the stepper motor BEFORE bringing CAN up, so a board that spends a
  // while retrying is never sitting with the driver energised.
  disable();

  // Give the MCP2515 crystal time to stabilise before the first attempt. Without
  // this the readback in begin() can fail purely because the oscillator has not
  // started yet -- which is why this failed at cold power-on but not on a reset.
  delay(CAN_SETTLE_MS);

  canUp = canTryBegin();
  lastCanTryMs = millis();
  if (canUp) canUpMs = millis();
}

void loop() {
  // 1. Fold any incoming command into the buffer. Frame is [steps:u32 BE][dir][rpm]:
  //    steps are ADDED (capped), rpm sets speed, dir 0x02=reverse / 0x01=forward /
  //    0x00=stop (clear). A direction change clears first so we never run backwards.
  // Only act on a REAL received packet. parsePacket() returns 0 when nothing
  // arrived, but packetId() stays stale (== canId) and readBytes() would then
  // return zeros -> buf[4]=0x00 would be misread as a STOP and wipe the buffer.
  // 0. Keep trying to bring the controller up. A single failed attempt at power-on
  //    must not leave this node deaf for the rest of the session -- and because the
  //    usual cause is the crystal not having settled yet, the retry almost always
  //    succeeds. Non-blocking: stepping and the LED keep running while it retries.
  if (!canUp && (millis() - lastCanTryMs) >= CAN_RETRY_MS) {
    lastCanTryMs = millis();
    canUp = canTryBegin();
    if (canUp) {
      canUpMs = millis();
      Serial.print("CAN up after ");
      Serial.print(canTries);
      Serial.println(" attempt(s)");
    }
  }

  int packetSize = canUp ? CAN.parsePacket() : 0;
  if(packetSize > 0) {
    long id = CAN.packetId();
    everRx = true;   // any frame at all proves the node can hear the bus
    uint8_t buf[8] = {0};
    int dlc = CAN.packetDlc();
    CAN.readBytes((char*)buf, dlc);

    if (id == canId) {
      // ---- message protection ---------------------------------------------
      // On a challenge build a command must be the 8-byte protected form,
      // [steps u32 BE][dir][rpm][nonce][crc8], with crc8 over bytes 0..6 keyed by
      // this motor's compiled-in Data ID. On a baseline build every frame is
      // accepted and the 6-byte legacy form still drives the car.
      //
      // REJECTION IS ABSOLUTELY SILENT: no error frame, no reply, no serial print,
      // no LED change, no counter a contestant can observe. Any difference between
      // "rejected" and "never arrived" is an ORACLE -- it would let someone
      // brute-force the CRC byte by watching for the frame that stops being
      // ignored, turning C2 from analysis into 256 transmissions.
      //
      // Short frames are rejected too. Accepting the unprotected form here would
      // make the whole scheme optional to an attacker.
      // Permissive until provisioned; strict once a Data ID has arrived.
      bool accept = true;
      if (haveDataId) {
        accept = (dlc == 8) && (crc8J1850Keyed(buf, 7, myDataId) == buf[7]);
      }

      if (accept) {
        long add = ((long)buf[0] << 24) | ((long)buf[1] << 16) | ((long)buf[2] << 8) | (long)buf[3];
        uint8_t d = buf[4];
        driveRpm = (dlc >= 6 && buf[5] > 0) ? buf[5] : rpm;   // optional speed byte
        if (driveRpm < 1) driveRpm = 1;

        if (d == 0x00) {
          stepsLeft = 0;                                    // explicit stop / clear
        } else {
          int newDir = (d == 0x02) ? BACKWARD : FORWARD;
          if (newDir != driveDir) stepsLeft = 0;            // direction change clears
          driveDir = newDir;
          stepsLeft += add;
          if (stepsLeft > STEPS_MAX) stepsLeft = STEPS_MAX; // cap coast/overrun
        }
      }
    } else if (id == NODE_CFG_ID) {
      // [type][idx][payload]. Accepted ONLY while unconfigured -- see the note at
      // the top: a node that could be reconfigured on the bus could be handed a
      // known Data ID by an attacker.
      if (!haveDataId && dlc >= 4 && buf[0] == 0x02) {
        cfgStore(((uint16_t)buf[2] << 8) | buf[3]);
        Serial.print("provisioned: Data ID stored, protection now ENFORCED\n");
      }
    } else if (id == HEARTBEAT_ID) {
      lastHeartbeatMs = millis();
      haveHeartbeat = true;
    } else if (id == BMS_BEACON_ID) {
      lastBeaconMs = millis();
      haveBeacon = true;
      // [v_hi][v_lo][pwr][flags]; a 2-byte (pre-beacon) BMS reports no state,
      // so treat it as "12V unknown" = not ready rather than assuming it is on.
      beacon12v = (dlc >= 3) && (buf[2] == 0x01);
    }
  }

  // 2. Drain the buffer, stepping at the commanded rpm; stay energised while there
  //    are steps left (smooth), disable when empty. Signed micros() diff wraps ok.
  if (stepsLeft > 0) {
    if (!driverOn) { enable(); driverOn = true; lastStepUs = micros(); }
    digitalWrite(dirPin, driveDir);
    long intervalUs = (60000000L / driveRpm) / stepsPerRev;   // microseconds per step
    if ((long)(micros() - lastStepUs) >= intervalUs) {
      lastStepUs = micros();
      digitalWrite(stepPin, HIGH);
      delayMicroseconds(3);
      digitalWrite(stepPin, LOW);
      stepsLeft--;
    }
  } else {
    if (driverOn) { disable(); driverOn = false; }
  }

  // 3. Report whole-system state on the onboard pixel.
  updateLed();

  // 4. And on the serial port, CONTINUOUSLY.
  reportStatus();
}

// reportStatus prints the node's view of the world every STATUS_MS.
//
// Deliberately periodic rather than once at boot. These are native-USB boards: a
// reset drops the CDC connection, so everything setup() prints is gone before a
// serial monitor can reattach -- which is exactly why the original one-shot
// "Failed to initialize CAN BUS." message was never seen by anyone. Attaching a
// monitor at ANY time must tell you the current state.
void reportStatus() {
  if (millis() - lastStatusMs < STATUS_MS) return;
  lastStatusMs = millis();

  Serial.print("id=0x");
  Serial.print(canId, HEX);
  if (!canUp) {
    Serial.print(" CAN=DOWN (begin() failed, ");
    Serial.print(canTries);
    Serial.print(" attempts) -- check MCP2515 wiring: CS=3 INT=0, SPI, and the ");
    Serial.print(CAN_CLOCK_HZ / 1000000);
    Serial.println("MHz crystal");
    return;
  }
  Serial.print(" CAN=up rx=");
  Serial.print(everRx ? "yes" : "NEVER");
  Serial.print(" hb=");
  Serial.print(heartbeatOK() ? "ok" : "MISSING");
  Serial.print(" 12v=");
  Serial.print(powerOK() ? "on" : "OFF");
  Serial.print(" steps=");
  Serial.println(stepsLeft);
}

void enable() {
  digitalWrite(enablePin, LOW);
}

void disable() {
  digitalWrite(enablePin, HIGH);
}
