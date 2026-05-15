"""
Tinytrek Web Software

This script sets up a Flask web application with WebSocket support using Flask-SocketIO.
It also interfaces with a CAN bus to send movement commands based on WebSocket events.

Modules:
    can: Provides Controller Area Network (CAN) support.
    engineio.async_drivers.gevent: Provides asynchronous support for Flask-SocketIO.
    flask: Provides the web framework.
    flask_socketio: Provides WebSocket support for Flask.
    signal: Provides mechanisms to handle signals.
    sys: Provides access to system-specific parameters and functions.

Functions:
    signal_handler(sig, frame): Handles the SIGINT signal to gracefully shut down the CAN bus.
    home(): Renders the home page.
    handle_move(direction): Handles WebSocket events for movement commands
                            and sends corresponding CAN messages.
"""

import signal
import sys

import can
from flask import Flask, render_template
from flask_socketio import SocketIO, emit

# Create an instance of the Flask class
app = Flask(__name__)
socketio = SocketIO(app, cors_allowed_origins='*')

# Set up CAN bus interface
bus = can.interface.Bus(channel='can1', interface='socketcan')


def signal_handler(sig, frame):
    """
    Handle the SIGINT signal to gracefully shut down the CAN bus.
    """
    if sig == signal.SIGINT:
        print('Exiting...')
        bus.shutdown()
        sys.exit(0)


# Define a route for the home page
@app.route('/')
def home():
    """
    Render the home page.
    """
    return render_template('index.html')


@socketio.on('power')
def handle_power(power):
    """
    Handle WebSocket events for power commands and send corresponding CAN messages.

    Args:
        power (str): The power command to execute.
    """
    if power.lower() == 'on':
        msg = can.Message(arbitration_id=0x115, data=[0x01], is_extended_id=False)
        bus.send(msg)
    elif power.lower() == 'off':
        msg = can.Message(arbitration_id=0x115, data=[0x02], is_extended_id=False)
        bus.send(msg)


# Define a WebSocket event handler for the 'connect' event
@socketio.on('move')
def handle_move(direction):
    """
    Handle WebSocket events for movement commands and send corresponding CAN messages.

    Args:
        direction (str): The direction to move the robot in.
    """
    directions = ['forward', 'backward', 'left', 'right']
    msg = None
    if direction.lower() in directions:
        # Create and send a CAN message depending on the direction
        if direction.lower() == 'forward':
            msg = can.Message(arbitration_id=0x111, data=[0x00, 0x00, 0x00, 0xFF, 0x01], is_extended_id=False)
            bus.send(msg)
            msg = can.Message(arbitration_id=0x113, data=[0x00, 0x00, 0x00, 0xFF, 0x01], is_extended_id=False)
            bus.send(msg)
        elif direction.lower() == 'backward':
            msg = can.Message(arbitration_id=0x111, data=[0x00, 0x00, 0x00, 0xFF, 0x02], is_extended_id=False)
            bus.send(msg)
            msg = can.Message(arbitration_id=0x113, data=[0x00, 0x00, 0x00, 0xFF, 0x02], is_extended_id=False)
            bus.send(msg)
        elif direction.lower() == 'left':
            msg = can.Message(arbitration_id=0x111, data=[0x00, 0x00, 0x00, 0xFF, 0x02], is_extended_id=False)
            bus.send(msg)
            msg = can.Message(arbitration_id=0x113, data=[0x00, 0x00, 0x00, 0xFF, 0x01], is_extended_id=False)
            bus.send(msg)
        elif direction.lower() == 'right':
            msg = can.Message(arbitration_id=0x111, data=[0x00, 0x00, 0x00, 0xFF, 0x01], is_extended_id=False)
            bus.send(msg)
            msg = can.Message(arbitration_id=0x113, data=[0x00, 0x00, 0x00, 0xFF, 0x02], is_extended_id=False)
            bus.send(msg)


def main():
    """
    Main function to run the Tinytrek web application.
    """
    # Run the app
    signal.signal(signal.SIGINT, signal_handler)
    socketio.run(app, debug=True, host="0.0.0.0")

# Run the app if the script is executed directly
if __name__ == '__main__':
    main()
