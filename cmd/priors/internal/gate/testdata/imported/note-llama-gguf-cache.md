---
name: note-llama-gguf-cache
description: where gguf model files are cached
metadata:
  type: project
---
Downloaded gguf files are cached under `~/.cache/models` and shared by the llama server and the benchmark. A model is evicted only when its file is removed by hand, and `du -sh ~/.cache/models` shows the total.
