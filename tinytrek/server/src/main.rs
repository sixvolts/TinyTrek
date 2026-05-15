use anyhow::Result;
use socketcan::{CanFrame, CanSocket, EmbeddedFrame, Socket, StandardId};
use tinytrek::prost::Message;
use tinytrek::{Direction, Msg, Movement};
use tokio::{io::AsyncReadExt, net::TcpListener};

fn move_dir(socket: &CanSocket, movement: Movement) {
    let mut lmotor = movement.steps.to_be_bytes().to_vec();

    let lmotor_dir = match movement.direction {
        Direction::Forward => 1,
        Direction::Backward => 2,
        Direction::Left => 2,
        Direction::Right => 1,
    };

    lmotor.push(lmotor_dir);

    let mut rmotor = movement.steps.to_be_bytes().to_vec();

    let rmotor_dir = match movement.direction {
        Direction::Forward => 1,
        Direction::Backward => 2,
        Direction::Left => 1,
        Direction::Right => 2,
    };

    rmotor.push(rmotor_dir);

    // Left Motor
    let lmotor_frame = CanFrame::new(StandardId::new(0x111).unwrap(), lmotor.as_slice()).unwrap();

    // Right Motor
    let rmotor_frame = CanFrame::new(StandardId::new(0x113).unwrap(), rmotor.as_slice()).unwrap();

    socket.write_frame(&lmotor_frame).unwrap();
    socket.write_frame(&rmotor_frame).unwrap();
}

#[tokio::main]
async fn main() -> Result<()> {
    let listener = TcpListener::bind("0.0.0.0:12345").await?;
    let socket = CanSocket::open("vcan0")?;

    tokio::spawn()
    loop {
        let (mut tcp_socket, _) = listener.accept().await?;

        let mut buf = Vec::new();
        tcp_socket.read_to_end(&mut buf).await?;

        if !buf.is_empty() {
            let msg = Msg::decode(buf.as_slice())?;
            let movement = Movement::from(msg);
            move_dir(&socket, movement);
        }
    }
}
