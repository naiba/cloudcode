import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Disable Next.js's built-in trailing-slash redirect so /instance/{id}/ is
  // forwarded as-is to the Go backend (which requires the trailing slash on its
  // /instance/{id}/ route). Without this, Next.js 308-redirects to strip the
  // slash before the rewrite runs, causing a redirect loop with Go's 301.
  skipTrailingSlashRedirect: true,
  // Dev: Next.js rewrites proxy API calls and WebSocket connections to Go backend.
  // Override the backend URL via NEXT_PUBLIC_BACKEND_URL (e.g. when running on :9090).
  async rewrites() {
    const backend = process.env.NEXT_PUBLIC_BACKEND_URL ?? "http://localhost:8080";
    return [
      {
        source: "/api/:path*",
        destination: `${backend}/api/:path*`,
      },
      {
        source: "/instances/:id/logs/ws",
        destination: `${backend}/instances/:id/logs/ws`,
      },
      {
        source: "/instances/:id/terminal/ws",
        destination: `${backend}/instances/:id/terminal/ws`,
      },
      {
        source: "/instance/:path*",
        destination: `${backend}/instance/:path*`,
      },
    ];
  },
  // Required for static export: disable image optimization
  images: {
    unoptimized: true,
  },
};

export default nextConfig;
