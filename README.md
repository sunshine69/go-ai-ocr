# go-ai-ocr

Scans an input directory for PDFs and images, rasterizes PDF pages via MuPDF
(`github.com/gen2brain/go-fitz`) at a configurable DPI (default 450), sends
each page/image to a vision model served OpenAI-style (e.g. `llama.cpp`
running Qwen2-VL), and writes the structured Markdown result to an output
directory laid out for a downstream RAG indexer to consume.

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
  -concurrency 4
```

`-endpoint` should be your `llama-server` (or any OpenAI-compatible server)
`/v1/chat/completions` route with vision support enabled.

## Output layout

```
<output>/manifest.jsonl                     # one JSON line per page/image: status, source, output path
<output>/<relpath>/<doc>/<doc>_page_0001.md # one file per PDF page
<output>/<relpath>/<image>/<image>.md       # one file per standalone image
```

Each `.md` file has a small YAML front matter block (source path, page
number, DPI, model, timestamp) followed by the model's Markdown output —
plain text an indexer can chunk directly, with provenance already attached.

## Flags

| Flag             | Default                                             | Purpose                                   |
|------------------|------------------------------------------------------|--------------------------------------------|
| `-input`         | *(required)*                                          | Directory to scan (recursive)             |
| `-output`        | *(required)*                                          | Where to write `.md` files + manifest     |
| `-dpi`           | 450                                                    | PDF rasterization DPI                     |
| `-endpoint`      | `http://127.0.0.1:8080/v1/chat/completions`           | Vision model endpoint                     |
| `-model`         | `qwen2-vl`                                            | Model name in request body                |
| `-api-key`       | `$go-ai-ocr_API_KEY`                                    | Bearer token, if the endpoint needs one   |
| `-prompt`        | built-in OCR/structuring prompt                       | Override the instruction sent per image   |
| `-concurrency`   | `NumCPU()`                                             | Parallel page/image workers               |
| `-timeout`       | 180s                                                    | Per-request HTTP timeout                  |
| `-max-retries`   | 2                                                       | Retries on transient endpoint failures    |
| `-skip-existing` | true                                                    | Resume: skip pages already written        |
| `-image-format`  | `png`                                                   | Rasterized page format (`png` or `jpeg`)  |
| `-jpeg-quality`  | 92                                                       | JPEG quality if `-image-format=jpeg`      |
| `-v`             | false                                                    | Verbose per-file logging                  |

## Notes

- Concurrency is per page/image, not per document — a 200-page PDF's pages
  are processed in parallel.
- Each worker opens its own MuPDF document handle; `go-fitz` documents are
  not safe to share across goroutines.
- Writes are atomic (`tmp` file + rename) so a killed run never leaves a
  half-written `.md` file behind.
- Not included on purpose: chunking/embedding/indexing into a vector store —
  that's the downstream RAG indexer's job, per the brief.
