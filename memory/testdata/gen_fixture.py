#!/usr/bin/env python3
"""Generate tokenizer parity fixtures from the HuggingFace reference implementation.

Throwaway: run once to produce testdata/tokenizer_parity.json, which the Go
tests consume. Not part of the build. Requires only the `tokenizers` wheel
(no deps) extracted onto PYTHONPATH.
"""
import json, os, sys
from tokenizers import Tokenizer

MODEL_DIR = os.path.expanduser("~/.cache/localchat/models/bge-small-en-v1.5")
MAX_LEN = 512

# Risk-targeted cases. Each name documents the specific failure mode it guards.
CASES = [
    ("basic",            "Hello world"),
    ("lowercasing",      "HELLO World MiXeD CaSe"),
    ("query_prefix",     "Represent this sentence for searching relevant passages: what is DuckDB?"),
    ("accents",          "Café naïve résumé über"),
    ("apostrophe",       "DuckDB's ART index isn't fixed"),
    ("wordpiece_cont",   "antidisestablishmentarianism unbelievability"),
    ("paths_idents",     "conf/skills/foo.md and tools_plan.go:42 plus snake_case_name"),
    ("whitespace",       "  multiple   spaces\tand\ttabs  \n"),
    ("cjk_emoji",        "emoji \U0001f389 and 中文字"),
    ("empty",            ""),
    ("punct_only",       "!!! ??? ---"),
    ("numbers",          "version 1.4.1 costs $19.99 on 2026-07-28"),
    ("markdown",         "## Heading\n\n- item `code_span()` **bold**"),
    ("wikilink",         "see [[Some Note#Section]] and [[alias|display]]"),
    ("repeated_subword", "tokenization tokenizer tokenizing tokenized"),
]


def main():
    tok_path = os.path.join(MODEL_DIR, "tokenizer.json")
    if not os.path.exists(tok_path):
        sys.exit(f"missing {tok_path}")

    # Untruncated reference: exact ids for normal-length inputs.
    plain = Tokenizer.from_file(tok_path)

    # Separate instance with truncation enabled, mirroring what our Go code must
    # do at 512. HF handles [SEP] placement correctly under truncation; a naive
    # slice of the id array would drop it, which is the bug this guards.
    trunc = Tokenizer.from_file(tok_path)
    trunc.enable_truncation(max_length=MAX_LEN)

    out = {
        "model_dir": MODEL_DIR,
        "max_len": MAX_LEN,
        "tokenizers_version": __import__("tokenizers").__version__,
        "cases": [],
    }

    for name, text in CASES:
        e = plain.encode(text)
        out["cases"].append({
            "name": name,
            "text": text,
            "ids": e.ids,
            "tokens": e.tokens,
            "type_ids": e.type_ids,
            "attention_mask": e.attention_mask,
        })

    # Long input: the only case where truncation actually engages.
    long_text = ("DuckDB is an in-process analytical database. " * 120).strip()
    e_full = plain.encode(long_text)
    e_trunc = trunc.encode(long_text)
    out["truncation"] = {
        "text": long_text,
        "untruncated_len": len(e_full.ids),
        "truncated_ids": e_trunc.ids,
        "truncated_tokens": e_trunc.tokens,
        "truncated_len": len(e_trunc.ids),
        "last_id": e_trunc.ids[-1],
        "first_id": e_trunc.ids[0],
    }

    dest = sys.argv[1] if len(sys.argv) > 1 else "tokenizer_parity.json"
    with open(dest, "w") as f:
        json.dump(out, f, indent=2, ensure_ascii=False)

    print(f"wrote {dest}")
    print(f"  tokenizers  {out['tokenizers_version']}")
    print(f"  cases       {len(out['cases'])}")
    print(f"  truncation  {out['truncation']['untruncated_len']} -> "
          f"{out['truncation']['truncated_len']} ids "
          f"(first={out['truncation']['first_id']}, last={out['truncation']['last_id']})")
    for c in out["cases"][:4]:
        print(f"  {c['name']:16s} {c['ids'][:12]}{'...' if len(c['ids']) > 12 else ''}")


if __name__ == "__main__":
    main()
