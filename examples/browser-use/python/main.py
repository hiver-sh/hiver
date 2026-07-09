import asyncio
import hiver
from playwright.async_api import async_playwright


async def main():
    sandbox = await hiver.get_or_create_sandbox("browser", hiver.SandboxConfig(image="browser"))

    # Build the CDP WebSocket URL: proxy URL for port 9223, http -> ws, + /cdp
    ws_endpoint = sandbox.proxy_url(9223).replace("http", "ws", 1) + "cdp"

    async with async_playwright() as p:
        browser = await p.chromium.connect_over_cdp(ws_endpoint)
        context = browser.contexts[0]
        page = context.pages[0] if context.pages else await context.new_page()

        await page.goto("https://news.ycombinator.com", wait_until="domcontentloaded")
        titles = await page.eval_on_selector_all(
            ".titleline > a", "els => els.map(e => e.textContent)"
        )
        print(titles)

        await browser.close()  # disconnects the client; the sandbox browser stays warm


asyncio.run(main())
