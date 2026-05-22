# Hive Sandbox Inspector

A local dev tool for listing, creating, shutting down, and inspecting events from Hive sandboxes.

## Usage

```sh
npx inspector
```

Run from the `inspector/` directory. This starts the proxy server on **port 3001** and the UI on **port 5173**, then opens the browser automatically.

## Configuration

The inspector connects to `http://localhost:9000` by default. Click the **⚙** icon in the top-right of the UI to override the controller URL.

To override the default at the server level, set `CONTROLLER_URL` before starting:

```sh
CONTROLLER_URL=http://my-controller:9000 npx inspector
```
