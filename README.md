# epub-summarizer

A command-line utility for chunking and summarizing EPUB-formatted books.

## Usage

```shell
env ANTHROPIC_API_KEY=$(cat ~/.anthropic-api-key) go run ./main.go some-book.epub --chunk-size=6500 --workers=3 --rate-limit=5 > /tmp/some-book.txt
```