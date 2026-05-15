use anyhow::Result;

use cursive::event::{Event, Key};
use cursive::views::TextView;

use tinytrek::Msg;
use tinytrek::prost::Message;

use tokio::net::TcpStream;
use tokio::sync::mpsc;

#[tokio::main]
async fn main() -> Result<()> {
    let mut siv = cursive::default();

    let socket = TcpStream::connect("127.0.0.1:12345").await?;

    let (tx, mut rx) = mpsc::channel::<Msg>(100);

    tokio::spawn(async move {
        use tokio::io::AsyncWriteExt;

        let mut socket = socket;

        while let Some(message) = rx.recv().await {
            let mut buf = Vec::new();
            message.encode(&mut buf).unwrap();
            if let Err(e) = socket.write_all(&mut buf).await {
                eprintln!("Failed to send message: {}", e);
                break;
            }
        }
    });

    siv.add_layer(TextView::new("Test"));

    siv.add_global_callback(Event::Key(Key::Up), move |_| {
        let tx = tx.clone();
        tokio::spawn(async move {
            let msg = Msg {
                steps: 200,
                direction: 0,
            };

            tx.send(msg).await.unwrap();
        });
    });

    siv.run();

    Ok(())
}
