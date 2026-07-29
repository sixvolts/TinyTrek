ENABLE_UART = "1"
ENABLE_SPI_BUS = "1"

do_deploy:append() {
    echo "arm_64bit=1" >> $CONFIG

    # Free the PL011 UART (ttyAMA0) for the serial ops console (§5.5). No downside here.
    echo "dtoverlay=disable-bt" >> $CONFIG

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
    echo "dtparam=spi=on" >> $CONFIG
    echo "dtoverlay=spi1-3cs" >> $CONFIG
    echo "dtoverlay=mcp251xfd,spi0-0,interrupt=25" >> $CONFIG
    echo "dtoverlay=mcp251xfd,spi1-0,interrupt=24" >> $CONFIG
}
