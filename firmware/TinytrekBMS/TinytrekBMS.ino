#include <CAN.h>

int canId = 0x115;

void setup() {
  Serial.begin(115200);

  Serial.println("Initializing GPIO");
  pinMode(A0, OUTPUT);
  pinMode(13, OUTPUT);

  CAN.setPins(PIN_CAN_CS, PIN_CAN_INTERRUPT);

  Serial.println("Initializing CAN");
  if (!CAN.begin(500000)) {
    Serial.println("Failure Initializing CAN.");
  }

  powerOff();
}

void loop() {
  int packetSize = CAN.parsePacket();

  if(CAN.packetId() == canId) {
    char buf[8] = {0};
    CAN.readBytes(buf, CAN.packetDlc());

    if(buf[0] == 0x1) {
      powerOn();
    } else if(buf[0] == 0x2) {
      powerOff();
    }
  }
}

void powerOn() {
  digitalWrite(A0, HIGH);
}

void powerOff() {
  digitalWrite(A0, LOW);
}
