# go-ai-ocr

Scans an input directory for PDFs and images, rasterizes PDF pages via MuPDF
(`github.com/gen2brain/go-fitz`) at a configurable DPI (default 450), and
submits them to a vision model served OpenAI-style (e.g. `llama.cpp` running
Qwen2-VL) in small, fixed-size batches. Each document's batches are
concatenated into a single Markdown file in an output directory laid out
for a downstream RAG indexer to consume.

**This is intentionally single-threaded, start to finish.** One document at
a time, one batch at a time, one blocking HTTP request at a time. There is
no worker pool and nothing to tune for concurrency — `-batch-size` is the
only knob, and it controls how many page images go into one request to the
model, not how many requests run at once (there is never more than one).

## Build

Requires cgo + a C toolchain (go-fitz bundles MuPDF's C sources).

```
go build -o go-ai-ocr .
```

## Run

```
./go-ai-ocr \
  -input  /path/to/docs \
  -output /path/to/index-ready \
  -endpoint http://127.0.0.1:8080/v1/chat/completions \
  -model qwen2-vl \
  -dpi 450 \
  -batch-size 2
```

`-endpoint` should be your `llama-server` (or any OpenAI-compatible server)
`/v1/chat/completions` route with vision support enabled.

## How a document is processed

For each PDF or image found under `-input`:

1. **Render.** A PDF's pages are rasterized one by one at `-dpi` into a
   temp directory (`page_0001.png`, `page_0002.png`, ...). A standalone
   image is used as-is — it's treated as a one-page document.
2. **Batch.** The page list is split into consecutive groups of
   `-batch-size` (default 2). A 10-page PDF with `-batch-size 2` makes 5
   batches; the last batch is however many pages are left over.
3. **Submit, one batch at a time.** Each batch's images are base64-encoded
   and sent together in a single chat-completion request (multiple
   `image_url` parts, one text prompt telling the model to mark each page
   with `<!-- page N -->` in its reply so the pages stay distinguishable).
   The next batch isn't started until this one's response comes back — no
   concurrency, no second request in flight.
4. **Save the part.** The model's Markdown response for that batch is
   written to `part_0001.md`, `part_0002.md`, ... in the temp directory.
5. **Concatenate.** Once every batch for the document has succeeded, all
   part files are concatenated in order, with a YAML front-matter header
   (source, page count, dpi, batch size, model, timestamp), into the
   final `<output>/<doc>.md`.
6. **Clean up.** The temp directory (rendered pages + part files) is
   deleted — unless `-keep-temp` is set, or the document failed, in which
   case it's left in place on purpose so you can see exactly what was
   rendered and which batch failed.

## Output layout

```
<output>/manifest.jsonl     # one JSON line per document: status, source, page_count, output path
<output>/<relpath>/<doc>.md # one final file per source document (all its pages concatenated)
```

## Flags

| Flag             | Default                                             | Purpose                                     |
|------------------|------------------------------------------------------|----------------------------------------------|
| `-input`         | *(required)*                                          | Directory to scan (recursive)               |
| `-output`        | *(required)*                                          | Where to write `.md` files + manifest       |
| `-dpi`           | 450                                                    | PDF rasterization DPI                       |
| `-endpoint`      | `http://127.0.0.1:8080/v1/chat/completions`           | Vision model endpoint                       |
| `-model`         | `qwen2-vl`                                            | Model name in request body                  |
| `-api-key`       | `$go-ai-ocr_API_KEY`                                    | Bearer token, if the endpoint needs one     |
| `-prompt`        | built-in OCR/structuring prompt                       | Override the base instruction               |
| `-batch-size`    | `2`                                                     | Consecutive pages sent per request — the whole point of this flag is to fit what your model can actually hold at once |
| `-timeout`       | 180s                                                    | Per-request HTTP timeout                    |
| `-max-retries`   | 2                                                       | Retries on a failed batch, same batch only  |
| `-skip-existing` | true                                                    | Resume: skip documents whose final `.md` already exists |
| `-image-format`  | `png`                                                   | Rasterized page format (`png` or `jpeg`)    |
| `-jpeg-quality`  | 92                                                       | JPEG quality if `-image-format=jpeg`        |
| `-keep-temp`     | false                                                    | Keep rendered pages + per-batch Markdown instead of deleting them |
| `-v`             | false                                                    | Verbose per-batch logging                   |

## Notes

- `-skip-existing` checks per document (the final `.md`), not per page —
  a partially-failed document has no final `.md`, so it's retried in full
  on the next run, not resumed mid-document.
- On failure, the error names the exact batch and page range that failed
  (e.g. `batch 3/5 (pages page_0005.png..page_0006.png)`), and that
  document's temp dir is kept regardless of `-keep-temp` so you can inspect
  or hand-retry it.
- Not included on purpose: chunking/embedding/indexing into a vector store
  — that's the downstream RAG indexer's job, per the brief.
