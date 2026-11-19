---
name: note-compose-ports
description: ports published by the dev compose file
metadata:
  type: reference
---
The dev compose file publishes the database on `5433` and the cache on `6380`, listed by `docker compose -f dev/compose.yml ps`. The app container reaches them by service name.
