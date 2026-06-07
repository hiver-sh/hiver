# Hiver CLI

Command-line tool for the [Hiver](https://hiver.sh) agent runtime — run a local
stack, bundle agent images, inspect live sandbox traffic, and stream events.

📖 Full documentation: [hiver.sh/docs](https://hiver.sh/docs)

## Getting started

```sh
npm install --global @hiver.sh/cli

# Or just use:
npx @hiver.sh/cli
```

### Chrome DevTools for Agents

Run `hiver inspect` to launch the inspector:

![Hiver DevTools](https://cdn.jsdelivr.net/npm/@hiver.sh/cli/docs/devtools.png)


## Requirements

- Node.js ≥ 18
- Docker — required by `up`, `down`, and `bundle` (the local stack and image
  bundling run as containers).
