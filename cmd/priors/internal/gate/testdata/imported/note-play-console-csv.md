---
name: note-play-console-csv
description: format of the play console CSV export
metadata:
  type: reference
---
The play console export has one row per day and localized column names. The import into the spreadsheet is done by `./scripts/import-csv.sh --locale en-US` because decimal commas otherwise break the totals.
