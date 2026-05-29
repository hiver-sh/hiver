import type { Meta, StoryObj } from "@storybook/react";
import { MemoryRouter } from "react-router-dom";
import { SandboxDetail } from "@/components/SandboxDetail";
import { TransportProvider } from "@/lib/transport";
import traceData from "./trace-sample.json";

const SANDBOX = {
  id: "sandbox-demo",
  endpoint: "http://sandbox-demo:8080",
};

const ARGS = {
  sandbox: SANDBOX,
  serverUrl: "http://localhost:3001",
  controllerUrl: "http://localhost:9000",
  onShutdown: () => {},
};

const meta: Meta<typeof SandboxDetail> = {
  title: "SandboxDetail",
  component: SandboxDetail,
  decorators: [
    (Story, { parameters }) => (
      <MemoryRouter>
        <TransportProvider traceData={traceData}>
          <div className={`h-screen bg-background text-foreground${parameters.dark ? " dark" : ""}`}>
            <Story />
          </div>
        </TransportProvider>
      </MemoryRouter>
    ),
  ],
};

export default meta;
type Story = StoryObj<typeof SandboxDetail>;

export const Default: Story = { args: ARGS };

export const Dark: Story = {
  args: ARGS,
  parameters: { dark: true },
};
