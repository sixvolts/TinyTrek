/*
This sample code is for testing the 2 stepper motors 
The rotation velocity can be adjusted by the code switch 
Microcontroller: Arduino UNO  
*/

#include <CAN.h>

const int dirPin = 4;
const int stepPin = 5;
const int enablePin = 6;

int FORWARD = HIGH;
int BACKWARD = LOW;

// Steps for one revolution on stepper motor
int stepsPerRev = 200;

// Desired speed
int rpm = 150;

int canId = 0x111;

void setup() {
  pinMode(dirPin, OUTPUT);
  pinMode(stepPin, OUTPUT);
  pinMode(enablePin, OUTPUT);

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
  int packetSize = CAN.parsePacket();

  if(CAN.packetId() == canId) {
    char buf[8] = {0};
    CAN.readBytes(buf, CAN.packetDlc());
    uint32_t steps = (buf[0] << 24) | (buf[1] << 16) | (buf[2] << 8) | buf[3];
    int dirVal = buf[4];
    int dir = FORWARD;

    if(dirVal == 0x2) {
      dir = BACKWARD;
    }

    doStep(steps, dir);
  }
}

void doStep(int steps, int dir) {
  enable();
  digitalWrite(dirPin, dir);
  for (int j = 0; j <= steps; j++) {
    digitalWrite(stepPin, LOW);
    delayMicroseconds(2);
    digitalWrite(stepPin, HIGH);
    delayMicroseconds(2);
    digitalWrite(stepPin, LOW);
    delay((60000/rpm)/stepsPerRev);
  }
  disable();
}

void enable() {
  digitalWrite(enablePin, LOW);
}

void disable() {
  digitalWrite(enablePin, HIGH);
}
