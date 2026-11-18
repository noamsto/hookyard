---
name: note-nginx-buffering
description: proxy buffering problem on the status page
metadata:
  type: project
---
The status page stalled because the reverse proxy buffered the event stream. The directive `proxy_buffering off` in `/etc/nginx/conf.d/status.conf` fixed it.
