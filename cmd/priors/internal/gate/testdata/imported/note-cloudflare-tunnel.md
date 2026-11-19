---
name: note-cloudflare-tunnel
description: cloudflared tunnel layout
metadata:
  type: reference
---
The tunnel `home` exposes two hostnames through one `cloudflared tunnel run home` service. The ingress rules are in `/etc/cloudflared/config.yml` and end with a catch-all 404.
