# PixPack

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue?style=for-the-badge)](LICENSE)
[![GitHub Stars](https://img.shields.io/github/stars/nam348tnh3gp/pixpack?style=for-the-badge&logo=github)](https://github.com/nam348tnh3gp/pixpack/stargazers)
[![GitHub Issues](https://img.shields.io/github/issues/nam348tnh3gp/pixpack?style=for-the-badge)](https://github.com/nam348tnh3gp/pixpack/issues)
[![Last Commit](https://img.shields.io/github/last-commit/nam348tnh3gp/pixpack?style=for-the-badge)](https://github.com/nam348tnh3gp/pixpack/commits)

**Encode any text, link, Wi-Fi login, image, or binary file into PNG images — and decode them back perfectly.**

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
| **Wi-Fi QR** | Join-network code for phones (WPA/WPA2/WPA3, WEP, open, hidden networks) |
| **Smart compression** | Parallel trial of gzip, zstd, xz, brotli, CMX1 (context mixing), LZW and LZH |
| **Lossless** | Full round-trip with CRC32 integrity checks |
| **Verified CMX1** | CMX1 output is decoded in parallel while it is produced and discarded if it does not round-trip |
| **Chunking** | Automatically splits large data across multiple images |
| **QR auto-size** | Grows the image when QR modules would be too small to scan |
| **Web UI** | Local web interface with live progress |
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

# Wi-Fi (scan with the phone camera to join)
./pixpack wifi MyNetwork -pass 12345678 -o wifi.png

# Image (lossless, may split into multiple images)
./pixpack image photo.jpg -o encoded.png

# Any binary file (default: densest Pixel PNG)
./pixpack file data.bin -o packed.png

# Force QR format
./pixpack file data.bin -fmt qr -o qr.png
```

### Wi-Fi QR

```bash
# WPA / WPA2 / WPA3 (default)
./pixpack wifi MyNetwork -pass 'my;pass:word' -o wifi.png

# Read the password from stdin so it does not end up in shell history
echo 'my password' | ./pixpack wifi MyNetwork -pass - -o wifi.png

# WEP, open network, hidden network
./pixpack wifi OldRouter -sec WEP -pass 1234567890 -o wep.png
./pixpack wifi CafeGuest -sec nopass -o open.png
./pixpack wifi Secret -pass 12345678 -hidden -o hidden.png
```

The QR contains the standard payload `WIFI:T:WPA;S:<ssid>;P:<password>;;`.
Special characters (`\ ; , : "`) are escaped automatically. WPA3 networks use the
`WPA` type, which phones accept. Wi-Fi codes are only available over `POST`
(the web form), never in a GET URL, so the password does not appear in server logs.

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
| **CMX1** | Text, source code, repetitive data | Custom context mixing (PAQ/lpaq-style). Smallest output, but the slowest |
| **brotli** | General text / code | Strong ratio at moderate speed |
| **zstd** | Speed + good ratio | Excellent balance |
| **xz** | Strong compression | Slower |
| **gzip** | Compatibility | Fast baseline |
| **LZH** | Fallback | Lempel-Ziv + Huffman |
| **LZW** | DNA-like sequences | Optimized for long runs of identical bytes |

On compressible data (source code, text, DNA), CMX1 usually gives the smallest result.
In one test on a real 1 MB text file, CMX1 produced about 19% less data than brotli,
at roughly twice the time. On already-compressed data (MP4, JPEG, ZIP), ratios stay close
to 100% and the slow algorithms are skipped automatically.

### CMX1

CMX1 is a bitwise context-mixing compressor with an arithmetic coder:

- order-0 to order-6 contexts, plus word and sparse contexts, in checked hash buckets
- a match model with learned confidence
- three mixers and a second mixing layer
- three APM/SSE stages

Output is deterministic, and every CMX1 stream is decoded again as it is created
(files up to 8 MB), so a corrupted result is never selected. CMX1 uses a fair amount of
RAM on large files (up to roughly 250 MB while verifying a file of 1 MB or more).

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
Text, URL and Wi-Fi modes always produce a single QR.

---

## CLI Options

```
pixpack <text|url|wifi|image|file> <content|path|ssid> [options]

General:
  -o string        Output file (default "out.png")
  -s int           Image size in pixels (default 768)

QR:
  -l string        Error correction: L, M, Q, H (default "M")
  -c int           Data characters per QR when splitting (default 900)
  -max int         Maximum number of QR images / parts
  -fit             For images: shrink + JPEG to fit in one QR
  -target-px int   Auto-size: minimum pixels per QR module, 4-16 (default 8)
  -no-auto         Disable auto-size (use -s exactly, may be unscannable)

Format (image / file):
  -fmt string      pixel, grid, qr (default "pixel")
  -depth int       Pixel: bits per channel (8 or 16)
  -ch int          Pixel: channels (3=RGB or 4=RGBA)
  -px int          Pixel: pixels per side
  -bits int        Grid: bits per channel (1-8)
  -cell int        Grid: pixels per cell
  -grid int        Grid: cells per side

Wi-Fi:
  -pass string     Password (use "-" to read it from stdin)
  -sec string      Security: WPA (default; also WPA2/WPA3), WEP, nopass
  -hidden          Mark the network as hidden

pixpack decode [-o dir] image1.png image2.png ...
pixpack serve [-addr :8090] [-maxmb N]
```

`-maxmb` limits the upload size of the web server in MB (`0` = unlimited).

**Auto-size.** If `-s` is too small for the chosen `-l` level, the image is grown so that
every QR module is at least `-target-px` pixels wide. Use `-no-auto` to keep `-s` exactly.

---

## Web UI

The built-in web server provides:
- Modes: text, URL, Wi-Fi, image, any file
- Live compression comparison table
- Progress bars for upload and processing
- Side-by-side algorithm results
- One-click download of single image or ZIP of all images
- Decode multiple images at once

For the Wi-Fi mode the password is sent to your own PixPack server only to draw the QR;
it is not stored.

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
