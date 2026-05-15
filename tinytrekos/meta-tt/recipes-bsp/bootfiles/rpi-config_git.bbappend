ENABLE_UART = "1"
ENABLE_SPI_BUS = "1"

do_deploy:append() {
    echo "arm_64bit=1" >> $CONFIG
    echo "dtoverlay=disable-bt" >> $CONFIG
    echo "dtoverlay=dwc2" >> $CONFIG
    echo "dtoverlay=spi1-3cs" >> $CONFIG
    echo "dtoverlay=mcp2515,spi1-1,oscillator=16000000,interrupt=22" >> $CONFIG
    echo "dtoverlay=mcp2515,spi1-2,oscillator=16000000,interrupt=13" >> $CONFIG
}
