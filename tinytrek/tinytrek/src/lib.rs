include!(concat!(env!("OUT_DIR"), "/tinytrek.rs"));

pub use prost;

pub struct Movement {
    pub steps: u32,
    pub direction: Direction,
}

impl Movement {
    pub fn new(steps: u32, direction: Direction) -> Self {
        Self { steps, direction }
    }
}

impl From<Msg> for Movement {
    fn from(msg: Msg) -> Self {
        Self {
            steps: msg.steps,
            direction: self::Direction::try_from(msg.direction).unwrap(),
        }
    }
}