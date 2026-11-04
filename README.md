# PixPack

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue?style=for-the-badge)](LICENSE)
[![GitHub Stars](https://img.shields.io/github/stars/nam348tnh3gp/pixpack?style=for-the-badge&logo=github)](https://github.com/nam348tnh3gp/pixpack/stargazers)
[![GitHub Issues](https://img.shields.io/github/issues/nam348tnh3gp/pixpack?style=for-the-badge)](https://github.com/nam348tnh3gp/pixpack/issues)
[![Last Commit](https://img.shields.io/github/last-commit/nam348tnh3gp/pixpack?style=for-the-badge)](https://github.com/nam348tnh3gp/pixpack/commits)

**Encode any text, link, image, or binary file into PNG images — and decode them back perfectly.**

PixPack turns data into visual form using three formats:
- **Pixel PNG** — densest (up to 8 bytes per pixel with 16-bit RGBA)
- **Color Grid** — high-density colored cells
- **QR Codes** — scannable by any phone camera

It automatically tries multiple compressors and picks the smallest result.

---

## Features

| Feature | Description |
|---------|-------------|
| **Multi-format output** | Pixel PNG, Color Grid, or QR codes |
| **Smart compression** | Parallel trial of gzip, zstd, xz, brotli, custom CMX1 (context-mixing), and LZW |
| **Lossless** | Full round-trip with CRC32 integrity checks |
| **Chunking** | Automatically splits large data across multiple images |
| **Web UI** | Beautiful local web interface with live progress |
| **CLI** | Fast command-line tool for scripting |

---

## Quick Start

### Install

```bash
git clone https://github.com/nam348tnh3gp/pixpack.git
cd pixpack
go mod tidy
go build -o pixpack
```

### Encode

```bash
# Text
./pixpack text "Hello, World!" -o hello.png

# URL
./pixpack url example.com -o link.png

# Image (lossless, may split into multiple images)
./pixpack image photo.jpg -o encoded.png

# Any binary file (default: densest Pixel PNG)
./pixpack file data.bin -o packed.png

# Force QR format
./pixpack file data.bin -fmt qr -o qr.png
```

### Decode

```bash
./pixpack decode -o restored/ packed.png
# or multiple images:
./pixpack decode -o restored/ packed_*.png
```

### Web Interface

```bash
./pixpack serve
# Open http://localhost:8090
```

---

## Compression Comparison

PixPack tries several algorithms in parallel and selects the smallest:

| Algorithm | Best for | Notes |
|-----------|----------|-------|
| **brotli** | General text / code | Often the winner on compressible data |
| **CMX1** | Structured / repetitive data | Custom context-mixing (PAQ-style) |
| **zstd** | Speed + good ratio | Excellent balance |
| **xz** | Strong compression | Slower |
| **gzip** | Compatibility | Fast baseline |
| **LZW** | DNA-like sequences | Optimized for long runs of identical bytes |

On highly compressible files (source code, text, DNA), CMX1 and brotli frequently lead. On already-compressed data (MP4, JPEG, ZIP), ratios stay close to 100% and faster algorithms are preferred.

---

## Formats

### Pixel PNG (default, densest)
Writes data directly into PNG pixels.  
16-bit RGBA = **8 bytes per pixel**. Only PixPack can read it.

### Color Grid
Each cell stores multiple bits in R/G/B channels.  
Configurable density vs robustness.

### QR Codes
Standard QR codes. Can be scanned by any phone, but lower density.  
Automatically splits large data across multiple QR images.

---

## CLI Options

```
pixpack <text|url|image|file> <input> [options]

Options:
  -o string     Output file (default "out.png")
  -fmt string   Format: pixel, grid, qr (default "pixel")
  -s int        Image size in pixels (default 768)
  -l string     QR error correction: L, M, Q, H (default "M")
  -fit          For images: shrink + JPEG to fit in one QR
  -depth int    Pixel: bits per channel (8 or 16)
  -ch int       Pixel: channels (3=RGB or 4=RGBA)
  -px int       Pixel: pixels per side
  -bits int     Grid: bits per channel (1-8)
  -cell int     Grid: pixels per cell
  -grid int     Grid: cells per side

pixpack decode [-o dir] image1.png image2.png ...
pixpack serve [-addr :8090] [-maxmb N]
```

---

## Web UI

The built-in web server provides:
- Live compression comparison table
- Progress bars for upload and processing
- Side-by-side algorithm results
- One-click download of single image or ZIP of all images
- Decode multiple images at once

---

## Integrity

Every encoded image includes:
- CRC32 checksum of the compressed payload
- Format magic (`QPX1`, `QCG1`, `QRG1`)
- Chunk index and total count

Decoding verifies the checksum before returning the original file.

---

## License

MIT

---

## Contributing

Issues and pull requests are welcome!

**Star the repo** if you find PixPack useful.
