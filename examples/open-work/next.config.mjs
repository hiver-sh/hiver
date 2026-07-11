/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The app is a single client page; /task/<id> is client-side routing state.
  // Rewrite it to / so a hard refresh at that URL still loads the app.
  async rewrites() {
    return [{ source: "/task/:id", destination: "/" }];
  },
};

export default nextConfig;
