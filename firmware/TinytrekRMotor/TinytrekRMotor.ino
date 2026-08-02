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

// --- Status NeoPixel (onboard; QT Py SAMD21 = pin 11, no power-gate pin) -----
//   GREEN  solid  : READY  -- heartbeat + 12V both good, not moving
//   YELLOW solid  : ACTIVE -- propulsion commanded and running
//   RED    solid  : no heartbeat AND no 12V   (both missing)
//   RED    double : no heartbeat from the Pi  (12V is fine)
//   RED    blink  : no 12V active             (heartbeat is fine)
#ifdef PIN_NEOPIXEL
Adafruit_NeoPixel pixel(1, PIN_NEOPIXEL, NEO_GRB + NEO_KHZ800);
#else
Adafruit_NeoPixel pixel(1, 11, NEO_GRB + NEO_KHZ800);
#endif
const uint8_t LED_BRIGHTNESS = 64;   // 0..255; bright enough to read, not blinding
const unsigned long LED_UPDATE_MS = 20;

unsigned long lastLedMs = 0;
uint32_t      ledLast   = 0xFFFFFFFF;   // force the first show()

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

  bool hb  = heartbeatOK();
  bool pwr = powerOK();
  uint8_t r, g, b;
  bool on = true;

  if (hb && pwr) {
    if (stepsLeft > 0) { r = 255; g = 180; b = 0; }   // YELLOW: propulsion active
    else               { r = 0;   g = 255; b = 0; }   // GREEN: ready, idle
  } else {
    r = 255; g = 0; b = 0;                            // RED: not ready
    if (!hb && !pwr) {
      on = true;                                      // solid: both missing
    } else if (!hb) {
      unsigned long t = millis() % 1200;              // double blink: no Pi heartbeat
      on = (t < 130) || (t >= 260 && t < 390);
    } else {
      on = (millis() % 900) < 450;                    // regular blink: no 12V
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

  Serial.begin(115200);

  CAN.setPins(3, 0);

  Serial.println("Initializing CAN BUS...");
  if(!CAN.begin(500000)) {
    Serial.println("Failed to initialize CAN BUS.");
  }

  // Disable the stepper motor
  disable();
}

void loop() {
  // 1. Fold any incoming command into the buffer. Frame is [steps:u32 BE][dir][rpm]:
  //    steps are ADDED (capped), rpm sets speed, dir 0x02=reverse / 0x01=forward /
  //    0x00=stop (clear). A direction change clears first so we never run backwards.
  // Only act on a REAL received packet. parsePacket() returns 0 when nothing
  // arrived, but packetId() stays stale (== canId) and readBytes() would then
  // return zeros -> buf[4]=0x00 would be misread as a STOP and wipe the buffer.
  int packetSize = CAN.parsePacket();
  if(packetSize > 0) {
    long id = CAN.packetId();
    uint8_t buf[8] = {0};
    int dlc = CAN.packetDlc();
    CAN.readBytes((char*)buf, dlc);

    if (id == canId) {
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
}

void enable() {
  digitalWrite(enablePin, LOW);
}

void disable() {
  digitalWrite(enablePin, HIGH);
}
