# Environment

- Pi runs inside a Docker container. The filesystem is ephemeral — data outside mounted volumes does not persist.

# Cloning repositories

- Always clone repositories into `/tmp` unless the user explicitly instructs otherwise.

# Named ports

- Named container ports (published at `klank new -p name=container-port`) are
  listed in `/ports` — one line per port: name, container port, and the
  localhost address where the host machine can reach it. Read `/ports` to
  find out where services you start inside this container are exposed on the
  host, and tell the user that address when they ask how to reach a service.

# Showing diagrams to the user

- The `show_diagram` tool opens a mermaid diagram in the user's browser via
  klankd: it encodes the mermaid source into a mermaid.live URL on the host
  machine and opens it in a browser there. Use it when a visual diagram
  (architecture, flow, state machine, sequence) would help the user. The
  tool result tells you whether the browser opened; the user sees the
  rendered diagram — you do not.
