ENABLE_UART = "1"
ENABLE_SPI_BUS = "1"

do_deploy:append() {
    echo "arm_64bit=1" >> $CONFIG

    # Free the PL011 UART (ttyAMA0) for the serial ops console (§5.5). No downside here.
    echo "dtoverlay=disable-bt" >> $CONFIG

    # USB CDC-ACM serial ops console on the USB-C port (§5.5 g_serial path).
    # This is the SERIAL gadget that §4.7 explicitly kept when the Ethernet/RNDIS
    # gadget was dropped -- NOT a network gadget. dr_mode=peripheral puts the USB-C
    # port in device mode so a laptop enumerates /dev/ttyACM* (mac/win/linux native).
    # See the ttos-usb-console recipe for module load + getty. TEMPORARY bench aid --
    # expected to be removed before the event.
    echo "dtoverlay=dwc2,dr_mode=peripheral" >> $CONFIG

    # --- Waveshare 2-CH CAN FD HAT Rev2.1, "Mode A" (factory default) -----------
    # Mode A = two channels on TWO INDEPENDENT SPI buses (spi0 + spi1).
    # Source of truth: https://www.waveshare.com/wiki/2-CH_CAN_FD_HAT (verbatim below)
    #   dtparam=spi=on
    #   dtoverlay=spi1-3cs
    #   dtoverlay=mcp251xfd,spi0-0,interrupt=25
    #   dtoverlay=mcp251xfd,spi1-0,interrupt=24
    # oscillator= is intentionally omitted: the mainline mcp251xfd overlay defaults
    # clock-frequency to 40 MHz, which matches the HAT's 40 MHz crystal (verified in
    # arch/arm/boot/dts/overlays/mcp251xfd-overlay.dts, line "clock-frequency = <40000000>").
    #
    # [VERIFY-ON-BOARD] Confirm the HAT is jumpered/resistored for Mode A (factory
    # default; Mode B moves four 0R resistors to share one SPI). Then confirm probing:
    #   dmesg | grep mcp251xfd
    #   ip -details link show can0 && ip -details link show can1
    # (dtparam=spi=on is already emitted by ENABLE_SPI_BUS="1" above.)
    echo "dtoverlay=spi1-3cs" >> $CONFIG
    echo "dtoverlay=mcp251xfd,spi0-0,interrupt=25" >> $CONFIG
    echo "dtoverlay=mcp251xfd,spi1-0,interrupt=24" >> $CONFIG
}
