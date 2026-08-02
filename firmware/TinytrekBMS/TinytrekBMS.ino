#include <CAN.h>
#include <Adafruit_NeoPixel.h>

// --- CAN IDs ---
int canId = 0x115;         // RX: power command  [0x01]=on / [0x02]=off
int statusId = 0x116;      // TX: battery telemetry (Vbat millivolts, uint16 big-endian)

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
