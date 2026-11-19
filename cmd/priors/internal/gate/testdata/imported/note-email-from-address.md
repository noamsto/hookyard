---
name: note-email-from-address
description: from-address used for outgoing mail
metadata:
  type: reference
---
Outgoing notifications leave from the no-reply address of the owner domain. The relay is verified with `dig +short TXT example.invalid` style SPF lookups, and no extra DNS change was needed.
