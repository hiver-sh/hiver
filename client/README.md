# Client API

## TypeScript Client for Node.js and Bun

```ts
import * as hive from 'hive';

const sandboxConfig : hive.SandboxConfig = {
    image: 'mcp-server',
    ttl: 1800,
    fs: [
        {
            backend: 'gdrive',
            mount: '/workspace',
            acls: [ { path: '/workspace/**', access: 'rw' } ]
        },
        {
            backend: 'local',
            mount: '/scratch',
            acls: [ { path: '/scratch/**', access: 'rw' } ]
        }
    ],
    egress: {
        allow: [
            {
                host: 'go.dev',
                methods: [ 'GET' ],
                paths: [ '/solutions/case-studies/*' ]
            }
        ]
    }
};

const sandbox = await hive.getOrCreateSandbox('hive-example', sandboxConfig);
const mcpServerUrl = sandbox.getUrl();

const logEvents = async () => {
    for await (const event of sandbox.getEventsStream()) {
        console.info('sandbox event', event);
    }
};

logEvents();

// Keep the sandbox running
setInterval(sandbox.ping, 10000);
```
