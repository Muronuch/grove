export default {
  server: {
    host: '0.0.0.0',
    port: 5173,
    // grove serves this dev server under <env>.<project>.localhost, so the
    // hostname is not known ahead of time.
    allowedHosts: true,
    hmr: { clientPort: 80 },
  },
};
