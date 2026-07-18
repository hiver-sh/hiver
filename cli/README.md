# Hiver CLI

Command-line tool for the [Hiver](https://hiver.sh) agent runtime — run a local
stack, bundle agent images, inspect live sandbox traffic, and stream events.

📖 Full documentation: [hiver.sh/docs](https://hiver.sh/docs)

## Getting started

```sh
npm install --global @hiver.sh/cli

# Or just use:
npx -y @hiver.sh/cli
```

### Commands
```sh
⬢ Hiver · Agent Runtime v0.1.19

  Usage: hiver <command> [options]

  Commands
    up       Bring up local stack
    down     Bring down local stack
    connect  Connect to stack
    start    Start a sandbox
    stop     Stop a sandbox
    shell    Open an interactive shell in a sandbox
    list     List the sandboxes
    events   Stream a sandbox's events live as they happen
    inspect  Launch the inspector
    bundle   Bundle a Docker image into a Hiver runtime image
```

### Hiver Inspector

Run `hiver inspect` to launch the inspector:

![Hiver DevTools](https://cdn.jsdelivr.net/npm/@hiver.sh/cli/docs/devtools.png)

## Requirements

- Node.js ≥ 18
- Docker — required by `up`, `down`, and `bundle` (the local stack and image
  bundling run as containers).
