# Model experiments

Which local models were tried for Plinth's Ollama-backed checks (the automatic review of a script and the DLP check),
and what came out. Results are from michael's setup, as noted on 2026-10-29.

## Setup

One 16 GB GPU, and one model that serves both the review and the DLP check. So a model has to fit in VRAM and answer
in reasonable time, not only be accurate.

## Results

| Model | Result |
|---|---|
| `gpt-oss:20b` | Accurate. About 1.5 minutes per run. Tight on VRAM. |
| `qwen2.5-coder:14b` | Fast, but wrong line numbers and invented findings. |
| `deepcoder:14b` | As accurate as `gpt-oss:20b` on two smoke scripts. |

Plinth checks that a review finding quotes a real line of the script, so a model that gets line numbers wrong or
invents findings (as `qwen2.5-coder:14b` did) produces findings Plinth cannot use.

## Not settled

- No final pick is recorded.
- `deepcoder:14b` was tried on only two smoke scripts, and no timing or VRAM figure was kept for it.
- Only Ollama models were tested; no other provider is covered here.
