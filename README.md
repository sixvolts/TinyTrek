# TinyTrek

**An open-source learning platform for teaching concepts from modern connected vehicles.**
TinyTrek is a small "vehicle" built from a collection of microcontrollers and a Raspberry Pi. It mirrors the kinds of systems you'd find in a real connected car: a battery management system, motor controllers, sensors, an over-the-air (OTA) update system, multiple on-vehicle communication busses, and embedded Linux. It was built to teach a Connected Vehicles special-topics course at the University of Tulsa in Fall 2024, and it's free for anyone to use in a class, project, or experiment of their own.

## Background

The idea grew out of an older grad-school project called the "cyber-physical test bed," a capture-the-flag style game played with small vehicles. The first version got handed off to another group of students and kept evolving. TinyTrek is the open-source rebuild of that idea: a design that's cheap to reproduce and easy to use for teaching and hands-on experiments.

## How it maps to a real vehicle

Each part of TinyTrek stands in for something you'd find in an actual car:

| TinyTrek part | Real-vehicle equivalent |
| ------------- | ----------------------- |
| Raspberry Pi 4 | Infotainment / telematics computer (the thing behind the touchscreen) |
| Motor microcontrollers | Motor / drive controllers |
| BMS microcontroller | Battery management system |
| CAN bus | In-vehicle communication network |
| Relay on the BMS board | High-voltage contactor |
| OTA system | Software update system |
| Ultrasonic sensor | Forward object detection / emergency braking |

## The vehicle

The body is 3D-printed and meant to be assembled by the student or researcher. A Raspberry Pi 4 is the main brain. Two small stepper motors run on off-the-shelf drivers, and each motor has its own microcontroller that talks to the Pi over CAN, a networking standard common in vehicles. Those motor microcontrollers just do simple stepper control, nothing fancy.

### Battery management (BMS)

We wanted the battery system to behave a bit like a real vehicle, so it gets its own dedicated microcontroller. It doesn't truly "manage" the battery so much as control the energy flowing through the vehicle.
Power comes from knock-off Milwaukee M12 batteries, chosen mostly for cost. We needed about a dozen of these vehicles for the class, and the M12 packs are plenty for the light loads involved. The M12 receptacles are printed from an existing design, with a power switch and battery-holder mount added on.
When the physical power switch is on, the BMS supplies 5V to power up the Pi and some of the microcontrollers. Motor power runs on 12V straight from the battery, but we didn't want that live all the time. A relay on the BMS board has to close first, simulating the **contactor** in an electric vehicle. This is what mimics the split between low-voltage and high-voltage systems, and it lets the platform demonstrate safety interlocks, like keeping motor power off while a software update is running.

### Motion control

Two independently controlled motors give the vehicle rudimentary **tank drive**. Instead of adding more wheels, there's a simple ball caster at the rear. That means TinyTrek can make sharp turns or rotate in place, and it keeps the cost down since wheels and motors are some of the pricier parts.

### Sensors

The platform is designed so you can bolt on sensors as needed. We built one as a proof of concept: an ultrasonic sensor that detects objects in front of the vehicle, with the idea of demonstrating a simple "emergency braking" behavior. There was too much to cover in a single semester, so the sensor got dropped from the actual course, but the files for the front sensor are included here in case they're useful for your project.

## Repository layout

| Folder | What's inside |
| ------ | ------------- |
| `Documents/` | Hardware design references (motor control board, power distribution board, wiring diagram) |
| `TinytrekBMS/` | Firmware for the battery/power-management microcontroller |
| `TinytrekLMotor/` | Firmware for the left motor controller |
| `TinytrekRMotor/` | Firmware for the right motor controller |
| `tinytrek/` | Vehicle control application (Rust) |
| `tinytrek_web/` | Browser-based dashboard (Python) |
| `tinytrekos/` | TinyTrekOS, the Raspberry Pi image built with Yocto |

## Build your own

All the design files and code live here on GitHub, and the printable parts are on Printables. If you want to make a TinyTrek for your own project or run a class with it, go for it.

* **Printable parts:** \<!-- TODO: add Printables link -->
* **Build video / walkthrough:** \<!-- TODO: add YouTube link -->

## Roadmap

The platform is built, the class has been taught, and the files are all up. We have more ideas for TinyTrek and will keep updating the repo as we go. Suggestions and contributions are welcome.

## Contributing

Open an issue or a pull request if you find a bug, want to add a feature, or have ideas for where TinyTrek should go next. If you build one or use it in a class, we'd love to hear about it.

## Credits

Built by the TinyTrek team for the Connected Vehicles course at the University of Tulsa, Fall 2024, including Andrew and Kyle.

## License

MIT
