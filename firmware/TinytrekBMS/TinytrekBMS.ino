#include <CAN.h>

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
const unsigned long TELEM_MS = 1000;              // telemetry period (~1 Hz)

// --- Low-voltage cutoff (protect a 3S pack) ---------------------------------
// Below CUTOFF the 12V rail is forced off; it stays locked out until the pack
// recovers above RECOVER (hysteresis stops it chattering near the threshold).
const long LVC_CUTOFF_MV  = 8500;  // cut 12V just above 0% (8.4V)
const long LVC_RECOVER_MV = 9000;  // re-allow power on above this

unsigned long lastTelem = 0;
bool lvcLockout = false;

void setup() {
  Serial.begin(115200);

  Serial.println("Initializing GPIO");
  pinMode(A0, OUTPUT);      // 12V rail control
  pinMode(13, OUTPUT);
  pinMode(BATT_PIN, INPUT); // battery sense

  CAN.setPins(PIN_CAN_CS, PIN_CAN_INTERRUPT);

  Serial.println("Initializing CAN");
  if (!CAN.begin(500000)) {
    Serial.println("Failure Initializing CAN.");
  }

  powerOff();
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

  // Read the pack every loop for a responsive low-voltage cutoff.
  long vbatMv = readVbatMv();
  if (vbatMv < LVC_CUTOFF_MV) {
    lvcLockout = true;
    powerOff();
  } else if (vbatMv > LVC_RECOVER_MV) {
    lvcLockout = false;
  }

  // Transmit battery telemetry at ~1 Hz.
  if (millis() - lastTelem >= TELEM_MS) {
    lastTelem = millis();
    CAN.beginPacket(statusId);
    CAN.write((vbatMv >> 8) & 0xFF);   // high byte
    CAN.write(vbatMv & 0xFF);          // low byte
    CAN.endPacket();
  }
}

void powerOn() {
  digitalWrite(A0, HIGH);
}

void powerOff() {
  digitalWrite(A0, LOW);
}
