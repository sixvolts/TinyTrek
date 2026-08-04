// Library sources live in this sketch folder so the Arduino IDE compiles them
// with no setup: open the .ino, pick the board, upload. QUOTED includes, not
// angle brackets -- the IDE adds the sketch directory to the quoted search
// path only, so <CAN.h> would not be found here.
#include "CAN.h"
#include "Adafruit_NeoPixel.h"

// --- CAN IDs ---
int canId = 0x115;         // RX: power command  [0x01]=on / [0x02]=off
int statusId = 0x116;      // TX: battery telemetry (Vbat millivolts, uint16 big-endian)

// ---- CTF challenge layer ---------------------------------------------------
// The BMS doubles as the DRIVE-bus MONITOR: it watches the motor command traffic it
// already receives and emits a flag frame on a signature no legitimate interface
// produces.
//
// NOTHING IS COMPILED IN. The Pi sends the unlock codes and detector thresholds
// during PROVISIONING -- an operator action, before the event -- and they are
// written to flash here. At runtime they are read from flash and nothing is
// transmitted, so the codes are never on the bus while a contestant is on it.
//
// A configured node IGNORES further config frames, so nobody who reaches the drive
// bus can overwrite them. Until provisioned this node detects nothing: an
// unprovisioned car has no challenge to protect, and staying quiet is the safe
// direction.
//
// The rules were prototyped and tuned against real timing on the bench before being
// brought here (bench/vehicle-emulator.py, bench/test-detectors.py) -- eight RP2040s
// are the slowest thing in this project to iterate on.
#include <EEPROM.h>

const long NODE_CFG_ID  = 0x101;   // RX: provisioning from the Pi
const uint8_t CFG_MAGIC = 0xC7;    // marks a written record; anything else = empty
const long HEARTBEAT_ID = 0x100;   // RX: [seq][armFlags]
const long L_MOTOR_ID   = 0x111;   // RX: monitored, never transmitted
const long R_MOTOR_ID   = 0x113;
const long C2_FLAG_ID   = 0x7D1;   // TX: C2 unlock code, 8 ASCII
const long C3_FLAG_ID   = 0x7D2;   // TX: C3 unlock code, 8 ASCII

// ARM MASK, mirrored from heartbeat byte 1. Bit SET = detector ARMED.
//
// POSITIVE ARMING, and the direction is the whole point. With redeemed-semantics a
// zero byte would mean ARMED, so any failure -- a Pi bug, a short frame, old
// firmware, a dropped heartbeat -- would leave detection live and the car would leak
// its unlock codes during ordinary driving. With arm-semantics those same failures
// leave detection DISARMED: the challenge does not fire, someone reports "this
// station is broken", and it gets fixed. A dead station is recoverable. A silently
// leaked flag is not.
const uint8_t ARM_C2 = 0x01;
const uint8_t ARM_C3 = 0x02;
const unsigned long HEARTBEAT_TIMEOUT_MS = 1000;  // stale heartbeat -> disarmed

const unsigned long HOLD_MS      = 1000;   // condition "still true" after last hit
const unsigned long FLAG_EMIT_MS = 500;    // 2 Hz while the condition holds
const uint8_t       C3_MAX       = 32;     // ring capacity; threshold is provisioned

// Provisioned by the Pi. Defaults are inert: with no codes there is nothing to
// emit, and detection stays off until cfgReady().
char          codeC2[9] = {0};
char          codeC3[9] = {0};
uint8_t       pivotRpm      = 0;     // the ONLY rpm legitimate locked traffic uses
unsigned long c2PairWindowMs = 250;
uint8_t       c3Count        = 15;
unsigned long c3WindowMs     = 3000;
bool          haveC2 = false, haveC3 = false, haveTuning = false;

// Which CHUNKS of each code have actually arrived: bit0 = chunk 0 (bytes 0-5),
// bit1 = chunk 1 (bytes 6-7). A code is complete only at 0x03.
//
// WHY THIS EXISTS. haveC2/haveC3 used to be set by the chunk-1 branch alone, so
// "I saw the last chunk" was taken to mean "I have the whole code". A node that
// missed chunk 0 -- exactly the case the Pi's 6-second repeat burst exists to
// cover, e.g. a replacement board still booting when provisioning ran -- would
// latch a code of six zero bytes plus two good ones, store it to EEPROM, and then
// refuse all further config because cfgReady() had flipped. It emits a garbage
// flag forever, and since cfgLoad() restores the latch from the magic byte it
// survives every reboot. There is no CAN or serial path to undo it: the board has
// to be physically erased. Chunks are cheap to count; do that instead.
uint8_t       c2Chunks = 0, c3Chunks = 0;

// Flash layout: [magic][C2 x8][C3 x8][pivotRpm][c2win/10][c3count][c3win/100]
static void cfgLoad() {
  EEPROM.begin(32);
  if (EEPROM.read(0) != CFG_MAGIC) return;
  for (uint8_t i = 0; i < 8; i++) { codeC2[i] = EEPROM.read(1 + i); codeC3[i] = EEPROM.read(9 + i); }
  pivotRpm       = EEPROM.read(17);
  c2PairWindowMs = (unsigned long)EEPROM.read(18) * 10;
  c3Count        = EEPROM.read(19); if (c3Count > C3_MAX) c3Count = C3_MAX;
  c3WindowMs     = (unsigned long)EEPROM.read(20) * 100;
  haveC2 = haveC3 = haveTuning = true;
  c2Chunks = c3Chunks = 0x03;   // a stored code is by definition complete
}

static void cfgStore() {
  EEPROM.write(0, CFG_MAGIC);
  for (uint8_t i = 0; i < 8; i++) { EEPROM.write(1 + i, codeC2[i]); EEPROM.write(9 + i, codeC3[i]); }
  EEPROM.write(17, pivotRpm);
  EEPROM.write(18, (uint8_t)(c2PairWindowMs / 10));
  EEPROM.write(19, c3Count);
  EEPROM.write(20, (uint8_t)(c3WindowMs / 100));
  EEPROM.commit();
}

static bool cfgReady() { return haveC2 && haveC3 && haveTuning && pivotRpm > 0; }

uint8_t       armMask = 0;
unsigned long lastHeartbeatMs = 0;
bool          haveHeartbeat = false;

uint8_t       lastDir[2]  = {0, 0};      // [0]=L, [1]=R
uint8_t       lastRpm[2]  = {0, 0};
unsigned long lastCmdMs[2] = {0, 0};

unsigned long c2HoldUntil = 0;
unsigned long c3HoldUntil = 0;
unsigned long c3Hits[C3_MAX];
uint8_t       c3Head = 0, c3Len = 0;
unsigned long lastFlagMs = 0;

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
  cfgLoad();   // provisioned values from flash; nothing is transmitted at runtime
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

// noteCommand folds one observed motor command into the detector state.
// idx 0 = left, 1 = right.
static void noteCommand(uint8_t idx, uint8_t dir, uint8_t rpm) {
  if (!cfgReady()) return;   // nothing provisioned -> nothing to detect
  unsigned long now = millis();
  lastDir[idx] = dir; lastRpm[idx] = rpm; lastCmdMs[idx] = now;
  if (dir == 0x00) { c3Len = 0; c3Head = 0; return; }   // an explicit stop breaks the run

  uint8_t other = idx ^ 1;
  bool paired      = lastCmdMs[other] != 0
                  && (now - lastCmdMs[other]) <= c2PairWindowMs;
  bool sameDirPair = paired && lastDir[other] == dir;

  // A PAIR IS CONSUMED ONCE EVALUATED. Without this, the partner timestamp stays
  // live and pairs again with whatever arrives next, so two legitimate commands
  // that were never issued together get judged as if they were.
  //
  // That was not theoretical. The C1 pivot sends L then R with OPPOSITE
  // directions, which is correctly not a translation. But call the routine twice
  // with opposite option bytes inside the 250 ms window and the boundary lines up:
  // the first call's R (dir 0x02) is still live when the second call's L (dir 0x02)
  // arrives, the directions match, and C2 fires. Sweeping the routine's option byte
  // from a script is the single most likely thing a team does after solving C1, so
  // the most obvious next move handed out the next challenge's code for free.
  //
  // Clearing both means a pair must be formed from two FRESH commands. A real
  // translation still fires on every L+R couple, so C3's consecutive-hit counting
  // is unchanged.
  if (paired) { lastCmdMs[0] = 0; lastCmdMs[1] = 0; }

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
  // ONLY EVALUATED PAIRS COUNT, and only they can break the run. The first command
  // of a pair has no partner to be same-direction WITH, so it is not yet evidence
  // either way. Letting it reach the else branch clears the run on every other
  // command -- and since a pair now consumes both timestamps, every second command
  // is a first-of-pair, so the hit count would never exceed one and C3 could never
  // fire at all.
  if ((armMask & ARM_C3) && paired) {
    if (sameDirPair && rpm != pivotRpm) {
      if (c3Len < c3Count) { c3Hits[(c3Head + c3Len) % C3_MAX] = now; c3Len++; }
      else { c3Hits[c3Head] = now; c3Head = (c3Head + 1) % C3_MAX; }
      if (c3Len >= c3Count && (now - c3Hits[c3Head]) <= c3WindowMs) {
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
  else if (packetSize > 0) {
    long rxId = CAN.packetId();
    int  rxDlc = CAN.packetDlc();
    uint8_t rb[8] = {0};
    CAN.readBytes((char*)rb, rxDlc);

    if (rxId == NODE_CFG_ID && rxDlc >= 2) {
      // [type][idx][payload]. Codes arrive as two 6-byte chunks.
      //
      // CODES are write-once: accepted only while unconfigured, so nothing on the
      // bus can rewrite the answers on a car that is already in play. TUNING is
      // NOT, deliberately -- see the 0x20 case.
      bool locked = cfgReady();
      bool dirty  = false;
      switch (rb[0]) {
        case 0x10:
          if (locked) break;
          if (rb[1] == 0) { memcpy(codeC2,     &rb[2], 6); c2Chunks |= 0x01; }
          else            { memcpy(codeC2 + 6, &rb[2], 2); c2Chunks |= 0x02; }
          haveC2 = (c2Chunks == 0x03);
          break;
        case 0x11:
          if (locked) break;
          if (rb[1] == 0) { memcpy(codeC3,     &rb[2], 6); c3Chunks |= 0x01; }
          else            { memcpy(codeC3 + 6, &rb[2], 2); c3Chunks |= 0x02; }
          haveC3 = (c3Chunks == 0x03);
          break;
        case 0x20:
          // TUNING IS ACCEPTED EVEN WHEN ALREADY PROVISIONED, and that is the point.
          //
          // The whole 0x101 handler used to be gated on !cfgReady(), so a provisioned
          // node silently discarded this record. The operator path says otherwise:
          // ttos-dashboard.default tells you to raise the pivot rpm if the car grinds
          // at 75, and ttos-provision-nodes says to re-run after changing it. The
          // burst went out, the script printed done, and the BMS kept the old value.
          //
          // That divergence is not cosmetic. C3's discriminator is "same direction AND
          // an rpm the pivot never uses". If the Pi pivots at 150 while this node still
          // believes 75, a team replaying captured pivot frames satisfies both halves
          // and collects the C3 code with no forgery at all -- defeating the property
          // ARCHITECTURE.md section 6 calls mechanical rather than policy.
          //
          // Safe to leave open: 0x101 is refused by the C3 relay's SEND path and is not
          // in the C2 bridge allowlist, so no contestant-reachable path can inject it.
          if (rxDlc >= 6) {
            uint8_t       newRpm   = rb[2];
            unsigned long newPair  = (unsigned long)rb[3] * 10;
            uint8_t       newCount = rb[4] > C3_MAX ? C3_MAX : rb[4];
            unsigned long newWin   = (unsigned long)rb[5] * 100;
            dirty = locked && (newRpm != pivotRpm || newPair != c2PairWindowMs ||
                               newCount != c3Count || newWin != c3WindowMs);
            pivotRpm       = newRpm;
            c2PairWindowMs = newPair;
            c3Count        = newCount;
            c3WindowMs     = newWin;
            haveTuning     = true;
          }
          break;
      }
      if (!locked && cfgReady()) {
        cfgStore();
        Serial.println("provisioned: codes and thresholds stored, detection ARMED");
      } else if (dirty) {
        // Persist a retune so it survives the next power cycle, exactly as the
        // original provisioning does.
        cfgStore();
        Serial.print("retuned: pivotRpm="); Serial.print(pivotRpm);
        Serial.print(" c2win="); Serial.print(c2PairWindowMs);
        Serial.print(" c3count="); Serial.print(c3Count);
        Serial.print(" c3win="); Serial.println(c3WindowMs);
      }
    } else if (rxId == HEARTBEAT_ID) {
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
    if (cfgReady()) {
      if ((armMask & ARM_C2) && millis() < c2HoldUntil) emitFlag(C2_FLAG_ID, codeC2);
      if ((armMask & ARM_C3) && millis() < c3HoldUntil) emitFlag(C3_FLAG_ID, codeC3);
    }
  }

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
