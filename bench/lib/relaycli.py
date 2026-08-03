import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
cmds = sys.stdin.read().splitlines()
s = socket.create_connection((host, port), timeout=10)
f = s.makefile("rwb")
ok = 0
# Read until the READY sentinel rather than counting banner lines -- the banner
# is documentation and will grow.
while True:
    line = f.readline()
    if not line or line.strip() == b"READY":
        break
for c in cmds:
    f.write((c + "\n").encode()); f.flush()
    if c == "QUIT": break
    r = f.readline().decode(errors="replace")
    if r.startswith("OK sent"): ok += 1
print(ok)
