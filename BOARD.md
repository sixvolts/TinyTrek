# TTOS CTF — board copy

Contestant-facing. Safe to print, project, or post. Nothing here reveals a solution.

The three are strictly sequential — each one's output is an input to the next — so
the board reads top to bottom.

---

## Category: **TINYTREK — a car that would rather you didn't**

Eight cars. Each one is a real vehicle network: two CAN buses, three microcontrollers,
a battery manager, and an operator panel that starts out locked. You get a diagnostic
port and the car's WiFi. Everything else you take.

Each challenge ends with a flag of the form `FLAG{XXXXXXXX}`. Paste it into the
car's panel at `http://192.168.244.1` and the panel unlocks more of itself; submit
it to the scoreboard for the points.

Some of the car's internal buses can only carry the eight characters without the
`FLAG{ }` around them — they are eight-byte buses and the wrapper does not fit. If
that is how you found it, that is a correct answer. The panel takes either form and
will show you the full flag to submit.

---

### Challenge 1 — **Do the Diag Dance**

The port under the dash isn't a fault-code reader. It's a live command interface, and
on this car nobody thought to guard it. Introduce yourself properly, ask the car what
it is, and then ask what it can *do* — one of the things it can do makes the wheels
turn. Nothing in this challenge is defended. The whole difficulty is knowing what to
say, in what order, and noticing that the car won't move at all until you've turned
something on first.

*Bring a CAN FD adapter. A classic-only one will transmit everything perfectly and
receive nothing, and a silent car looks exactly like a broken one.*

---

### Challenge 2 — **Ignore the Bus Behind the Curtain**

The messages you actually want live on a second bus, and you can't see it from the
port. There is a routine that advertises itself as opening a path between the two.
It is locked. It will stay locked no matter what session you're in or how nicely you
ask — that one's a decoy, and it's there because reading names is not the same as
reading behaviour.

Somewhere else on this car is a maintenance function that has to move the wheels to
do its job. While it's running, so can you. But you can't sniff what to send, and the
only commands the car will hand over are ones it has already issued — so replaying
them just makes it do what it already did. Getting it to do something it has *never*
done, using only messages it signed itself, is the challenge.

---

### Challenge 3 — **All Your Trek Are Belong to Us**

There's a network service listening on the car's WiFi. Everything it needs to
authenticate you is already in your hands by now — plus one last piece the car hands
to every browser that loads its front page, in the place vendors have been leaving it
for fifteen years.

Past the login you're on the internal bus with nothing between you and the motors but
a single protection byte. Get it wrong and the car ignores the command silently — the
service still says `OK`, the wheels just don't move. Replay what the car already said
and you'll only ever drive where the car already drove. To pick your own speed and
your own distance you'll have to work out what that byte is protecting, how, and with
what — and the answer is not the one you'll expect when you find it.

*The battery manager is watching the whole time. It will tell you when you've won.*

---

## Board footer / rules card

- **The car is yours for the duration.** Power-cycling it resets it to locked and
  costs you nothing but your unlocked panel tabs — the codes stay valid.
- **The panel unlocks progressively.** Each code you redeem gives you visibility or
  capability you didn't have. Redeem them as you get them; a later challenge may want
  what an earlier one showed you.
- **One car per team, and stay on your own car's WiFi.** Eight cars in one room means
  eight networks — check the SSID on your placard.
- **Use Safari or Firefox for the panel.** Chrome force-upgrades the URL to HTTPS and
  the panel will look dead.
- **Anything you can reach is in scope.** Denial of service, physically opening the
  car, and touching another team's vehicle are not.
- **If a car needs to stop, say so.** An operator can cut its drive rail instantly.
