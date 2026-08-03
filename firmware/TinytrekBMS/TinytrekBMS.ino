#include <CAN.h>
#include <Adafruit_NeoPixel.h>

// --- CAN IDs ---
int canId = 0x115;         // RX: power command  [0x01]=on / [0x02]=off
int statusId = 0x116;      // TX: battery telemetry (Vbat millivolts, uint16 big-endian)

// ---- CTF challenge layer ---------------------------------------------------
// The BMS doubles as the DRIVE-bus MONITOR. It watches the motor command traffic
// it is already receiving and emits a flag frame when it sees a signature no
// legitimate interface produces. Nothing here transmits unless a detector is both
// ARMED and triggered, and a baseline build compiles all of it out.
//
// These rules were prototyped and tuned on the bench against real timing before
// being brought here (bench/vehicle-emulator.py, bench/test-detectors.py) --
// eight RP2040s are the slowest thing in this project to iterate on.
#if defined(__has_include)
#  if __has_include("ttos-fleet.h")
#    include "ttos-fleet.h"
#  endif
#endif
#ifndef TTOS_CHALLENGE
#  define TTOS_CHALLENGE 0
#endif

#if TTOS_CHALLENGE
const long HEARTBEAT_ID = 0x100;   // RX: [seq][armFlags]
const long L_MOTOR_ID   = 0x111;   // RX: monitored, never transmitted
const long R_MOTOR_ID   = 0x113;
const long C2_FLAG_ID   = 0x7D1;   // TX: C2 unlock code, 8 ASCII
const long C3_FLAG_ID   = 0x7D2;   // TX: C3 unlock code, 8 ASCII

// ARM MASK, mirrored from heartbeat byte 1. Bit SET = detector ARMED.
//
// POSITIVE ARMING, not "redeemed" bits, and the direction is the whole point.
// With redeemed-semantics a zero byte would mean ARMED, so any failure -- a Pi
// bug, a short frame, old firmware, a dropped heartbeat -- would leave detection
// live and the car would leak its unlock codes during ordinary driving. With
// arm-semantics those same failures leave detection DISARMED: the challenge does
// not fire, someone reports "this station is broken", and it gets fixed.
// A dead station is recoverable. A silently leaked flag is not.
const uint8_t ARM_C2 = 0x01;
const uint8_t ARM_C3 = 0x02;
const unsigned long HEARTBEAT_TIMEOUT_MS = 1000;  // stale heartbeat -> disarmed

// Detection thresholds. C3_COUNT/C3_WINDOW_MS are specified; C2_PAIR_WINDOW_MS is
// not, and was chosen on the bench: the Pi commands both wheels inside one 150 ms
// keepalive cycle, so 250 ms covers a real pair with margin while staying too tight
// to accidentally pair two unrelated single-wheel commands.
const unsigned long C2_PAIR_WINDOW_MS = 250;
const unsigned long HOLD_MS           = 1000;   // condition "still true" after last hit
const uint8_t       C3_COUNT          = 15;     // consecutive qualifying commands
const unsigned long C3_WINDOW_MS      = 3000;
const uint8_t       PIVOT_RPM         = 75;     // the ONLY rpm legitimate locked traffic uses
                                                // MUST MATCH TTOS_PIVOT_RPM on the Pi. If they diverge,
                                                // the car fires 0x7D2 on its OWN pivot routine and leaks
                                                // the C3 code before anyone attempts the challenge.
const unsigned long FLAG_EMIT_MS      = 500;    // 2 Hz while the condition holds

uint8_t       armMask = 0;
unsigned long lastHeartbeatMs = 0;
bool          haveHeartbeat = false;

uint8_t       lastDir[2]  = {0, 0};      // [0]=L, [1]=R
uint8_t       lastRpm[2]  = {0, 0};
unsigned long lastCmdMs[2] = {0, 0};

unsigned long c2HoldUntil = 0;
unsigned long c3HoldUntil = 0;
unsigned long c3Hits[C3_COUNT];          // ring of qualifying-command timestamps
uint8_t       c3Head = 0, c3Len = 0;
unsigned long lastFlagMs = 0;
#endif  // TTOS_CHALLENGE

// --- Battery sense (A1) ------------------------------------------------------
// 120k high-side / 40.2k low-side divider (0.5%), fed to A1.
//   Vadc = Vbat * 40.2 / (120 + 40.2)  ->  Vbat = Vadc * (160.2 / 40.2) = Vadc * 3.985
// ADC_REF_MV is the analog reference in millivolts. The node is a 3.3V board
// (RP2040 / Feather RP2040 CAN), and Arduino analogRead defaults to 10-bit.
// Fine-tune ADC_REF_MV against a multimeter reading if the gauge is slightly off.
const int   BATT_PIN    = A1;
const long  ADC_REF_MV  = 3300;   // 3.3V board (RP2040). 5V board (UNO) -> 5000.
const int   ADC_MAX     = 1023;   // 10-bit analogRead default
const float DIVIDER     = (120.0 + 40.2) / 40.2;  // = 3.985
// Beacon period. This message is not just the battery gauge: the motor nodes use
// it as the "12V rail is really on" signal and light red without it, so it runs at
// 5 Hz (their timeout is 1 s = 5 missed beacons) rather than the old 1 Hz.
const unsigned long TELEM_MS = 200;               // beacon period (~5 Hz)

// --- Pack range / gauge bands -----------------------------------------------
// Same 3S range the dashboard maps: 8.4 V = 0%, 12.3 V = 100%.
//   50% -> 8400 + 0.50*(12300-8400) = 10350 mV
//   25% -> 8400 + 0.25*(12300-8400) =  9375 mV
const long BATT_MIN_MV  = 8400;    // 0%
const long BATT_MAX_MV  = 12300;   // 100%
const long BATT_50_MV   = BATT_MIN_MV + (BATT_MAX_MV - BATT_MIN_MV) * 50 / 100;  // 10350
const long BATT_25_MV   = BATT_MIN_MV + (BATT_MAX_MV - BATT_MIN_MV) * 25 / 100;  //  9375

// --- Low-voltage cutoff (protect a 3S pack) ---------------------------------
// Below CUTOFF the 12V rail is forced off; it stays locked out until the pack
// recovers above RECOVER (hysteresis stops it chattering near the threshold).
const long LVC_CUTOFF_MV  = 8500;  // cut 12V just above 0% (8.4V)
const long LVC_RECOVER_MV = 9000;  // re-allow power on above this
// Debounce: motor inrush briefly sags the pack when the 12V rail energizes, so a
// single sub-cutoff sample must NOT latch lockout. The pack has to stay below
// cutoff continuously for this long before the rail is cut.
const unsigned long LVC_TRIP_MS = 1500;

// --- Status NeoPixel (onboard on the Feather RP2040 CAN) --------------------
// Blink encodes the 12V relay; color encodes the battery band. Fast blink is
// reserved for the low-voltage cutoff (see the table in firmware/README.md).
//   solid       = 12V inactive        slow blink = 12V active (enabled)
//   green >=50%  amber 25..50%  red <25%      fast-blink red = below cutoff
#ifdef PIN_NEOPIXEL
Adafruit_NeoPixel pixel(1, PIN_NEOPIXEL, NEO_GRB + NEO_KHZ800);
#else
// Fall back to the Adafruit stock pin if the variant doesn't define one.
Adafruit_NeoPixel pixel(1, 16, NEO_GRB + NEO_KHZ800);
#endif
const uint8_t LED_BRIGHTNESS = 64;       // 0..255; keep it non-blinding
const unsigned long BLINK_SLOW_MS = 600; // "enabled" half-period
const unsigned long BLINK_FAST_MS = 150; // "cutoff" half-period
const unsigned long LED_UPDATE_MS = 25;  // don't hammer show()

unsigned long lastTelem  = 0;
unsigned long lastLed    = 0;
unsigned long belowSince = 0;            // when the pack first dropped below cutoff (0 = not below)
uint32_t      ledLast    = 0xFFFFFFFF;   // force first show()
bool lvcLockout = false;
bool power12v   = false;                 // tracks the 12V relay for the LED

void setup() {
  Serial.begin(115200);

  Serial.println("Initializing GPIO");
  pinMode(A0, OUTPUT);      // 12V rail control
  pinMode(13, OUTPUT);
  pinMode(BATT_PIN, INPUT); // battery sense

  // Some Adafruit RP2040 variants gate NeoPixel power behind a pin.
#ifdef NEOPIXEL_POWER
  pinMode(NEOPIXEL_POWER, OUTPUT);
  digitalWrite(NEOPIXEL_POWER, HIGH);
#endif
  pixel.begin();
  pixel.setBrightness(LED_BRIGHTNESS);
  pixel.clear();
  pixel.show();

  CAN.setPins(PIN_CAN_CS, PIN_CAN_INTERRUPT);

  Serial.println("Initializing CAN");
  if (!CAN.begin(500000)) {
    Serial.println("Failure Initializing CAN.");
  }

  powerOff();
}

// updateLed maps (12V state, battery band, cutoff) onto the onboard pixel.
// Non-blocking: throttled to LED_UPDATE_MS and only calls show() on a change.
void updateLed(long vbatMv) {
  if (millis() - lastLed < LED_UPDATE_MS) return;
  lastLed = millis();

  uint8_t r, g, b;
  unsigned long period;   // 0 = solid

  if (lvcLockout) {
    r = 255; g = 0; b = 0;            // below cutoff: fast-blink red, 12V off
    period = BLINK_FAST_MS;
  } else {
    if (vbatMv >= BATT_50_MV)      { r = 0;   g = 255; b = 0; }   // green
    else if (vbatMv >= BATT_25_MV) { r = 255; g = 120; b = 0; }   // amber
    else                           { r = 255; g = 0;   b = 0; }   // red
    period = power12v ? BLINK_SLOW_MS : 0;                        // slow if enabled
  }

  bool on = (period == 0) ? true : (((millis() / period) & 1UL) == 0);
  uint32_t c = on ? pixel.Color(r, g, b) : 0;
  if (c != ledLast) {
    pixel.setPixelColor(0, c);
    pixel.show();
    ledLast = c;
  }
}

// readVbatMv averages a few samples to steady the reading.
long readVbatMv() {
  long acc = 0;
  for (int i = 0; i < 8; i++) acc += analogRead(BATT_PIN);
  long adc = acc / 8;
  long vadcMv = adc * ADC_REF_MV / ADC_MAX;      // millivolts at A1
  long vbatMv = (long)(vadcMv * DIVIDER + 0.5);  // millivolts at the pack
  if (vbatMv < 0) vbatMv = 0;
  if (vbatMv > 65535) vbatMv = 65535;
  return vbatMv;
}

#if TTOS_CHALLENGE
// noteCommand folds one observed motor command into the detector state.
// idx 0 = left, 1 = right.
static void noteCommand(uint8_t idx, uint8_t dir, uint8_t rpm) {
  unsigned long now = millis();
  lastDir[idx] = dir; lastRpm[idx] = rpm; lastCmdMs[idx] = now;
  if (dir == 0x00) { c3Len = 0; c3Head = 0; return; }   // an explicit stop breaks the run

  uint8_t other = idx ^ 1;
  bool sameDirPair = lastCmdMs[other] != 0
                  && lastDir[other] == dir
                  && (now - lastCmdMs[other]) <= C2_PAIR_WINDOW_MS;

  // C2: both wheels commanded the SAME way inside a short window. That is a
  // translation, and no legitimate interface commands one while the panel is
  // locked -- the pivot always drives the wheels in OPPOSITE directions.
  if ((armMask & ARM_C2) && sameDirPair) c2HoldUntil = now + HOLD_MS;

  // C3: SUSTAINED same-dir commanding at an rpm legitimate traffic never uses.
  //
  // The same-dir condition is NOT optional. Keying on rpm alone passes every
  // positive test and then fires 0x7D2 on the car's OWN pivot routine the moment
  // the pivot rpm is changed -- leaking the C3 code before anyone attempts the
  // challenge. It is rpm AND direction, always.
  if (armMask & ARM_C3) {
    if (sameDirPair && rpm != PIVOT_RPM) {
      if (c3Len < C3_COUNT) { c3Hits[(c3Head + c3Len) % C3_COUNT] = now; c3Len++; }
      else { c3Hits[c3Head] = now; c3Head = (c3Head + 1) % C3_COUNT; }
      if (c3Len == C3_COUNT && (now - c3Hits[c3Head]) <= C3_WINDOW_MS) {
        c3HoldUntil = now + HOLD_MS;
      }
    } else {
      c3Len = 0; c3Head = 0;    // "CONSECUTIVE": anything else breaks the run
    }
  }
}

static void emitFlag(long id, const char* code) {
  CAN.beginPacket(id);
  for (uint8_t i = 0; i < 8; i++) CAN.write(code[i] ? code[i] : 0x00);
  CAN.endPacket();
}
#endif

void loop() {
  int packetSize = CAN.parsePacket();

  if(CAN.packetId() == canId) {
    char buf[8] = {0};
    CAN.readBytes(buf, CAN.packetDlc());

    if(buf[0] == 0x1) {
      if(!lvcLockout) powerOn();   // refuse power-on while the pack is too low
    } else if(buf[0] == 0x2) {
      powerOff();
    }
  }
#if TTOS_CHALLENGE
  else if (packetSize > 0) {
    long rxId = CAN.packetId();
    int  rxDlc = CAN.packetDlc();
    uint8_t rb[8] = {0};
    CAN.readBytes((char*)rb, rxDlc);

    if (rxId == HEARTBEAT_ID) {
      lastHeartbeatMs = millis();
      haveHeartbeat = true;
      // DLC < 2 means no flags field, so no arming information -> DISARMED.
      armMask = (rxDlc >= 2) ? rb[1] : 0;
    } else if (rxId == L_MOTOR_ID || rxId == R_MOTOR_ID) {
      // Monitor ONLY. The BMS never validates the CRC and never acts on these --
      // it is watching the shape of the traffic, not obeying it. Frames the motors
      // will silently reject still count, which is correct: a contestant hammering
      // the bus with bad CRCs is still commanding, and the signature is the intent.
      if (rxDlc >= 6) noteCommand(rxId == L_MOTOR_ID ? 0 : 1, rb[4], rb[5]);
    }
  }

  // Stale heartbeat -> disarm. Fail-safe direction, same 1 s timeout the motor
  // nodes already use.
  if (haveHeartbeat && (millis() - lastHeartbeatMs) >= HEARTBEAT_TIMEOUT_MS) armMask = 0;

  // Emit at 2 Hz while a condition holds, rather than once when it trips. A
  // single-shot emission would make the challenge a coin flip on whether the
  // contestant happened to be capturing at that instant; repeating it means
  // someone who solved it without a sniffer running can simply do it again.
  if (millis() - lastFlagMs >= FLAG_EMIT_MS) {
    lastFlagMs = millis();
    if ((armMask & ARM_C2) && millis() < c2HoldUntil) emitFlag(C2_FLAG_ID, TTOS_CODE_C2);
    if ((armMask & ARM_C3) && millis() < c3HoldUntil) emitFlag(C3_FLAG_ID, TTOS_CODE_C3);
  }
#endif

  // Low-voltage cutoff, debounced so motor-inrush sag doesn't cut the rail.
  // Latch only after the pack stays below cutoff for LVC_TRIP_MS continuously;
  // clear via hysteresis once it recovers above RECOVER.
  long vbatMv = readVbatMv();
  if (vbatMv < LVC_CUTOFF_MV) {
    if (belowSince == 0) belowSince = millis();
    if (!lvcLockout && (millis() - belowSince >= LVC_TRIP_MS)) {
      lvcLockout = true;
      powerOff();
      Serial.print("LVC: cutoff latched, 12V off at "); Serial.print(vbatMv); Serial.println(" mV");
    }
  } else {
    belowSince = 0;                       // back above cutoff: reset the debounce timer
    if (lvcLockout && vbatMv > LVC_RECOVER_MV) {
      lvcLockout = false;
      Serial.print("LVC: recovered at "); Serial.print(vbatMv); Serial.println(" mV");
    }
  }

  // Beacon: [v_hi][v_lo][pwr][flags] at ~5 Hz.
  //   pwr   0x01 = 12V rail on, 0x02 = off (same encoding as the 0x115 command)
  //   flags bit0 = low-voltage cutoff latched (explains WHY the rail is off)
  // The motor nodes gate their READY (green) state on this message, so it must
  // keep flowing whenever the BMS is alive.
  if (millis() - lastTelem >= TELEM_MS) {
    lastTelem = millis();
    CAN.beginPacket(statusId);
    CAN.write((vbatMv >> 8) & 0xFF);   // high byte
    CAN.write(vbatMv & 0xFF);          // low byte
    CAN.write(power12v ? 0x01 : 0x02); // actual rail state (not the commanded one)
    CAN.write(lvcLockout ? 0x01 : 0x00);
    CAN.endPacket();
  }

  // Drive the status pixel from the current 12V + battery + cutoff state.
  updateLed(vbatMv);
}

void powerOn() {
  digitalWrite(A0, HIGH);
  power12v = true;
}

void powerOff() {
  digitalWrite(A0, LOW);
  power12v = false;
}
