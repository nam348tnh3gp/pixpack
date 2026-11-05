// pixpack - encode text, links, images, or any binary file into images
// (pixel PNG / color grid / QR) and decode them back.
//
// Install:  go mod tidy && go build -o pixpack
//
// Encode:
//
//	./pixpack text "Hello world" -o hello.png
//	./pixpack url example.com -o link.png
//	./pixpack image photo.jpg -o p.png          # lossless, auto-split into multiple QRs if large
//	./pixpack image photo.jpg -fit -o p.png     # shrink + JPEG compress to fit 1 QR
//	./pixpack file data.bin -o d.png            # any binary, compressed (gzip/zstd/lzw/lzh/xz/brotli), auto-split
//	./pixpack file data.bin -o d.png            # default -fmt pixel: write directly into 16-bit RGBA PNG pixels (8 bytes/px)
//	./pixpack file data.bin -fmt grid|qr ...    # color grid / QR (scannable by phone)
//
// Decode (restore original file from QR images):
//
//	./pixpack decode -o out/ d_*.png
//
// Web server (port 8090):
//
//	./pixpack serve [-addr :8090]
package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/makiuchi-d/gozxing"
	gzqr "github.com/makiuchi-d/gozxing/qrcode"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/ulikunitz/xz"
)

const (
	maxDirectLen = 2000  // max characters for a single QR (text/url/image -fit)
	maxChunks    = 65535 // header format limit (4 hex chars), not a soft cap
	headerLen    = 20    // "QRG1" + total(4) + idx(4) + crc(8)
)

// serverMaxBytes: upload size limit for the server (0 = unlimited).
var serverMaxBytes int64

func limitBody(w http.ResponseWriter, r *http.Request) {
	if serverMaxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, serverMaxBytes+(64<<10))
	}
}

type opts struct {
	RL        qrcode.RecoveryLevel
	Size      int
	Chunk     int
	MaxChunks int
	Fit       bool
	Format    string // "pixel" or "grid"
	Bits      int    // grid: bits per color channel (1-8)
	Cell      int    // grid: pixels per cell
	GridW     int    // grid: cells per side
	Depth     int    // pixel: 8 or 16 bits per channel
	Channels  int    // pixel: 3 (RGB) or 4 (RGBA)
	PixSide   int    // pixel: pixels per image side

	WifiSSID   string // wifi mode: network name
	WifiPass   string // wifi mode: password
	WifiSec    string // wifi mode: WPA, WEP or nopass
	WifiHidden bool   // wifi mode: hidden network

	TargetPx int  // auto-size: min pixels per QR module (default 8)
	NoAuto   bool // disable auto-size

	Report func(map[string]any) // receives progress events (may be nil)
}

func (o opts) emit(e map[string]any) {
	if o.Report != nil {
		o.Report(e)
	}
}

// ---------- Base45 (RFC 9285, leverages QR alphanumeric mode) ----------

const b45 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

func b45enc(d []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(d); i += 2 {
		n := int(d[i])*256 + int(d[i+1])
		sb.WriteByte(b45[n%45])
		sb.WriteByte(b45[(n/45)%45])
		sb.WriteByte(b45[n/2025])
	}
	if len(d)%2 == 1 {
		n := int(d[len(d)-1])
		sb.WriteByte(b45[n%45])
		sb.WriteByte(b45[n/45])
	}
	return sb.String()
}

func b45dec(s string) ([]byte, error) {
	vals := make([]int, len(s))
	for i := 0; i < len(s); i++ {
		v := strings.IndexByte(b45, s[i])
		if v < 0 {
			return nil, fmt.Errorf("invalid base45 character at position %d", i)
		}
		vals[i] = v
	}
	var out []byte
	i := 0
	for ; i+2 < len(vals); i += 3 {
		n := vals[i] + vals[i+1]*45 + vals[i+2]*2025
		if n > 0xFFFF {
			return nil, errors.New("corrupted base45 data")
		}
		out = append(out, byte(n>>8), byte(n))
	}
	switch len(vals) - i {
	case 0:
	case 2:
		n := vals[i] + vals[i+1]*45
		if n > 0xFF {
			return nil, errors.New("corrupted base45 data")
		}
		out = append(out, byte(n))
	default:
		return nil, errors.New("invalid base45 length")
	}
	return out, nil
}

// ---------- Packaging ----------

func pack(name string, data []byte) ([]byte, error) { return packWith(name, data, nil) }

var precompressedExt = map[string]bool{
	".3gp": true, ".mp4": true, ".mkv": true, ".avi": true, ".mov": true,
	".webm": true, ".flv": true, ".wmv": true, ".m4v": true, ".mpg": true,
	".mp3": true, ".aac": true, ".ogg": true, ".flac": true, ".m4a": true,
	".opus": true, ".wma": true, ".wav": true, ".aiff": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".heic": true, ".heif": true, ".avif": true, ".bmp": true, ".tiff": true,
	".tif": true, ".ico": true,
	".zip": true, ".apk": true, ".jar": true, ".aar": true, ".war": true,
	".rar": true, ".7z": true, ".gz": true, ".bz2": true, ".xz": true,
	".zst": true, ".lz4": true, ".lzma": true, ".tgz": true, ".tbz2": true,
	".txz": true, ".tar.gz": true, ".tar.bz2": true, ".tar.xz": true,
	".cab": true, ".deb": true, ".rpm": true, ".dmg": true, ".iso": true,
	".whl": true, ".egg": true,
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true, ".epub": true, ".mobi": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true,
	".class": true, ".dex": true, ".wasm": true,
}

var precompressedMagic = [][]byte{
	{0x1F, 0x8B},
	{0x50, 0x4B, 0x03, 0x04},
	{0x50, 0x4B, 0x05, 0x06},
	{0x50, 0x4B, 0x07, 0x08},
	{0x89, 0x50, 0x4E, 0x47},
	{0xFF, 0xD8, 0xFF},
	{0x47, 0x49, 0x46, 0x38},
	{0x66, 0x74, 0x79, 0x70},
	{0x49, 0x44, 0x33},
	{0x4F, 0x67, 0x67, 0x53},
	{0x66, 0x4C, 0x61, 0x43},
	{0x25, 0x50, 0x44, 0x46},
	{0x52, 0x61, 0x72, 0x21},
	{0x37, 0x7A, 0xBC, 0xAF},
	{0xFD, '7', 'z', 'X', 'Z'},
	{0x28, 0xB5, 0x2F, 0xFD},
	{0x04, 0x22, 0x4D, 0x18},
	{0x1A, 0x45, 0xDF, 0xA3},
}

func looksPrecompressed(name string, data []byte) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".gz" || ext == ".bz2" || ext == ".xz" || ext == ".zst" || ext == ".lz4" {
		return true
	}
	if precompressedExt[ext] {
		return true
	}
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	for _, m := range precompressedMagic {
		if bytes.HasPrefix(data, m) {
			return true
		}
	}
	return false
}

func packWith(name string, data []byte, report func(map[string]any)) ([]byte, error) {
	emit := func(e map[string]any) {
		if report != nil {
			report(e)
		}
	}
	if len(name) > 65535 {
		name = name[:65535]
	}
	raw := make([]byte, 0, 2+len(name)+len(data))
	raw = append(raw, byte(len(name)>>8), byte(len(name)))
	raw = append(raw, name...)
	raw = append(raw, data...)

	precompressed := looksPrecompressed(name, data)
	big := len(raw) > 64<<20
	noSlow := precompressed
	huge := len(raw) > 8<<20
	isDNA := looksDNA(data)
	total := 9
	if isDNA {
		total += 4 // dna2-pack, dna2-zstd, dna2-lzh, dna2-brotli
	}
	emit(map[string]any{"type": "packstart", "total": total, "orig": len(data), "precompressed": precompressed})

	var mu sync.Mutex
	var best []byte
	bestName := ""
	var wg sync.WaitGroup
	run := func(label string, f func() []byte) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			var b []byte
			func() {
				defer func() {
					if recover() != nil {
						b = nil
					}
				}()
				b = f()
			}()
			ms := time.Since(t0).Milliseconds()
			if b == nil {
				emit(map[string]any{"type": "algo", "name": label, "failed": true, "ms": ms})
				return
			}
			emit(map[string]any{"type": "algo", "name": label, "size": len(b), "ms": ms})
			mu.Lock()
			if best == nil || len(b) < len(best) {
				best, bestName = b, label
			}
			mu.Unlock()
		}()
	}

	// DNA mode: pack 2-bit first, then compress the wrapped output.
	// Wrapped = [name_len][name][DNA2-packed-content]
	if isDNA {
		packed := packDNA2Bit(data)
		wrapped := make([]byte, 0, 2+len(name)+len(packed))
		wrapped = append(wrapped, byte(len(name)>>8), byte(len(name)))
		wrapped = append(wrapped, name...)
		wrapped = append(wrapped, packed...)

		run("dna2-pack", func() []byte {
			out := make([]byte, 0, 5+len(wrapped))
			out = append(out, 'D', 'N', 'A', '3', 'p')
			out = append(out, wrapped...)
			return out
		})
		run("dna2-zstd", func() []byte {
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
			if err != nil {
				return nil
			}
			defer enc.Close()
			inner := enc.EncodeAll(wrapped, nil)
			out := make([]byte, 0, 5+len(inner))
			out = append(out, 'D', 'N', 'A', '3', 'z')
			out = append(out, inner...)
			return out
		})
		run("dna2-lzh", func() []byte {
			inner := lzhCompress(wrapped)
			if inner == nil {
				return nil
			}
			out := make([]byte, 0, 5+len(inner))
			out = append(out, 'D', 'N', 'A', '3', 'l')
			out = append(out, inner...)
			return out
		})
		run("dna2-brotli", func() []byte {
			var buf bytes.Buffer
			bw := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
			if _, err := bw.Write(wrapped); err != nil || bw.Close() != nil {
				return nil
			}
			out := make([]byte, 0, 5+buf.Len())
			out = append(out, 'D', 'N', 'A', '3', 'b')
			out = append(out, buf.Bytes()...)
			return out
		})
	}

	run("gzip", func() []byte {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil
		}
		zw.Write(raw)
		if zw.Close() != nil {
			return nil
		}
		return buf.Bytes()
	})
	run("zstd", func() []byte {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil
		}
		defer enc.Close()
		return enc.EncodeAll(raw, nil)
	})
	if lzwLooksIncompressible(raw) {
		emit(map[string]any{"type": "algo", "name": "lzw", "skipped": true, "reason": "incompressible"})
		emit(map[string]any{"type": "algo", "name": "lzw-parallel", "skipped": true, "reason": "incompressible"})
		emit(map[string]any{"type": "algo", "name": "lzh", "skipped": true, "reason": "incompressible"})
		emit(map[string]any{"type": "algo", "name": "lzw-preset", "skipped": true, "reason": "incompressible"})
	} else if len(raw) >= lzwParallelMinSize {
		run("lzw-parallel", func() []byte {
			return lzwCompressParallel(raw)
		})
		run("lzh", func() []byte {
			return lzhCompress(raw)
		})
		run("lzw-preset", func() []byte {
			return lzwCompressWithPreset(raw)
		})
		emit(map[string]any{"type": "algo", "name": "lzw", "skipped": true, "reason": "use_parallel"})
	} else {
		run("lzw", func() []byte {
			return lzwCompress(raw)
		})
		run("lzh", func() []byte {
			return lzhCompress(raw)
		})
		run("lzw-preset", func() []byte {
			return lzwCompressWithPreset(raw)
		})
		emit(map[string]any{"type": "algo", "name": "lzw-parallel", "skipped": true, "reason": "use_single"})
	}
	if !big && !noSlow {
		run("xz", func() []byte {
			var buf bytes.Buffer
			xw, err := xz.NewWriter(&buf)
			if err != nil {
				return nil
			}
			if _, err := xw.Write(raw); err != nil || xw.Close() != nil {
				return nil
			}
			return buf.Bytes()
		})
		run("brotli", func() []byte {
			var buf bytes.Buffer
			buf.WriteString("BRO1")
			bw := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
			if _, err := bw.Write(raw); err != nil || bw.Close() != nil {
				return nil
			}
			return buf.Bytes()
		})
	} else {
		r := "too_large"
		if noSlow {
			r = "precompressed"
		}
		emit(map[string]any{"type": "algo", "name": "xz", "skipped": true, "reason": r})
		emit(map[string]any{"type": "algo", "name": "brotli", "skipped": true, "reason": r})
	}
	if !huge && !noSlow {
		run("CMX1", func() []byte {
			if len(raw) > 8<<20 {
				return cmCompress(raw)
			}
			c, ok := cmCompressVerified(raw)
			if !ok {
				return nil
			}
			return c
		})
	} else {
		r := "too_large"
		if noSlow {
			r = "precompressed"
		}
		emit(map[string]any{"type": "algo", "name": "CMX1", "skipped": true, "reason": r})
	}
	wg.Wait()
	if best == nil {
		return nil, errors.New("compression failed")
	}
	emit(map[string]any{"type": "pack", "orig": len(data), "packed": len(best), "algo": bestName})
	emit(map[string]any{"type": "stage", "msg": "Creating images"})
	return best, nil
}

func unpack(comp []byte) (string, []byte, string, error) {
	// DNA3 fast path: [name][compressed(DNA2-packed)]
	if len(comp) >= 5 {
		magic := string(comp[:5])
		if magic == "DNA3p" || magic == "DNA3z" || magic == "DNA3l" || magic == "DNA3b" {
			var inner []byte
			var innerErr error
			var innerAlgo string
			switch magic {
			case "DNA3p":
				innerAlgo = "dna2-pack"
				inner = comp[5:]
			case "DNA3z":
				innerAlgo = "dna2-zstd"
				dec, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
				if e != nil {
					return "", nil, innerAlgo, e
				}
				inner, innerErr = dec.DecodeAll(comp[5:], nil)
				dec.Close()
			case "DNA3l":
				innerAlgo = "dna2-lzh"
				inner, innerErr = lzhDecompress(comp[5:])
			case "DNA3b":
				innerAlgo = "dna2-brotli"
				inner, innerErr = io.ReadAll(brotli.NewReader(bytes.NewReader(comp[5:])))
			}
			if innerErr != nil {
				return "", nil, innerAlgo, fmt.Errorf("dna3 decompress: %w", innerErr)
			}
			if len(inner) < 2 {
				return "", nil, innerAlgo, errors.New("dna3: inner too short")
			}
			n := int(binary.BigEndian.Uint16(inner[:2]))
			if len(inner) < 2+n {
				return "", nil, innerAlgo, errors.New("dna3: name corrupt")
			}
			nm := string(inner[2 : 2+n])
			data, e := unpackDNA2Bit(inner[2+n:])
			if e != nil {
				return "", nil, innerAlgo, e
			}
			return nm, data, innerAlgo, nil
		}
	}

	var raw []byte
	var err error
	algo := "gzip"
	switch {
	case bytes.HasPrefix(comp, []byte("LZH1")):
		algo = "lzh"
		raw, err = lzhDecompress(comp)
	case bytes.HasPrefix(comp, []byte("LZW2")):
		algo = "lzw-preset"
		raw, err = lzwDecompressWithPreset(comp)
	case bytes.HasPrefix(comp, []byte{0x28, 0xB5, 0x2F, 0xFD}):
		algo = "zstd"
		var dec *zstd.Decoder
		dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err == nil {
			raw, err = dec.DecodeAll(comp, nil)
			dec.Close()
		}
	case bytes.HasPrefix(comp, []byte("LZW1")), bytes.HasPrefix(comp, []byte("LZWS")), bytes.HasPrefix(comp, []byte("LZWP")):
		algo = "lzw"
		raw, err = lzwDecompress(comp)
	case bytes.HasPrefix(comp, []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}):
		algo = "xz"
		var xr *xz.Reader
		xr, err = xz.NewReader(bytes.NewReader(comp))
		if err == nil {
			raw, err = io.ReadAll(xr)
		}
	case bytes.HasPrefix(comp, []byte("BRO1")):
		algo = "brotli"
		raw, err = io.ReadAll(brotli.NewReader(bytes.NewReader(comp[4:])))
	case bytes.HasPrefix(comp, []byte("CMX1")):
		algo = "CMX1"
		raw, err = cmDecompress(comp)
	default:
		var zr *gzip.Reader
		zr, err = gzip.NewReader(bytes.NewReader(comp))
		if err == nil {
			raw, err = io.ReadAll(zr)
		}
	}
	if err != nil {
		return "", nil, algo, fmt.Errorf("decompression error: %w", err)
	}
	if len(raw) < 2 {
		return "", nil, algo, errors.New("data too short")
	}
	n := int(binary.BigEndian.Uint16(raw[:2]))
	if len(raw) < 2+n {
		return "", nil, algo, errors.New("data corrupted")
	}
	return string(raw[2 : 2+n]), raw[2+n:], algo, nil
}

func chunkStrings(name string, data []byte, o opts) ([]string, error) {
	comp, err := packWith(name, data, o.Report)
	if err != nil {
		return nil, err
	}
	s := b45enc(comp)
	total := (len(s) + o.Chunk - 1) / o.Chunk
	if total > o.MaxChunks {
		return nil, fmt.Errorf("data too large: need %d QR codes (max %d)", total, o.MaxChunks)
	}
	crc := crc32.ChecksumIEEE(comp)
	out := make([]string, 0, total)
	for i := 0; i < total; i++ {
		end := (i + 1) * o.Chunk
		if end > len(s) {
			end = len(s)
		}
		out = append(out, fmt.Sprintf("QRG1%04X%04X%08X%s", total, i+1, crc, s[i*o.Chunk:end]))
	}
	return out, nil
}

// ---------- Image shrink ----------

func resize(src image.Image, nw, nh int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			ra, ga, ba, aa := r/n, g/n, bl/n, a/n
			white := 0xffff - aa
			i := dst.PixOffset(x, y)
			dst.Pix[i] = uint8((ra + white) >> 8)
			dst.Pix[i+1] = uint8((ga + white) >> 8)
			dst.Pix[i+2] = uint8((ba + white) >> 8)
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

func fitImage(data []byte, maxLen int) (string, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("cannot read image: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1.0
	if m := max(w, h); m > 320 {
		scale = 320 / float64(m)
		src = resize(src, max(int(float64(w)*scale), 4), max(int(float64(h)*scale), 4))
		b = src.Bounds()
		w, h = b.Dx(), b.Dy()
	}
	for scale = 1.0; scale > 0.03; scale *= 0.8 {
		nw, nh := max(int(float64(w)*scale), 4), max(int(float64(h)*scale), 4)
		img := resize(src, nw, nh)
		for _, q := range []int{50, 30, 15} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
				return "", err
			}
			uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
			if len(uri) <= maxLen {
				return uri, nil
			}
		}
	}
	return "", errors.New("cannot shrink image enough to fit in 1 QR")
}

// ---------- Encoding ----------

func parseLevel(l string) qrcode.RecoveryLevel {
	switch strings.ToUpper(l) {
	case "L":
		return qrcode.Low
	case "Q":
		return qrcode.High
	case "H":
		return qrcode.Highest
	}
	return qrcode.Medium
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL: %q", raw)
	}
	return u.String(), nil
}

// autoQRSize returns the minimum image size (px) such that each QR
// module is at least targetModulePx pixels wide. Uses q.Bitmap() to
// get the exact module count — no pixel scanning, no guessing.
//
// The QR image produced by skip2/go-qrcode always contains the standard
// 4-module quiet zone on each side, so total side = (modules+8)*k for
// some integer k >= 1. We pick the size so that k >= targetModulePx.
func autoQRSize(text string, level qrcode.RecoveryLevel, targetModulePx int) (size, modules int, err error) {
	if targetModulePx <= 0 {
		targetModulePx = 8
	}
	q, err := qrcode.New(text, level)
	if err != nil {
		return 0, 0, fmt.Errorf("cannot encode QR (content too large for level %v): %w", level, err)
	}
	modules = len(q.Bitmap()) // exact: 17 + 4*version
	totalMods := modules + 8  // +4 quiet zone on each side
	needed := totalMods * targetModulePx
	if needed < 128 {
		needed = 128
	}
	if needed > 4096 {
		needed = 4096
	}
	return needed, modules, nil
}

// oneWithAutoSize encodes a single text/url string. If o.Size is too
// small for the chosen error-correction level, it is increased so every
// module is >= o.TargetPx pixels. Disabled when o.NoAuto is true.
func oneWithAutoSize(text string, o opts) ([][]byte, error) {
	actualSize := o.Size
	if !o.NoAuto {
		needed, modules, err := autoQRSize(text, o.RL, o.TargetPx)
		if err != nil {
			return nil, err
		}
		if needed > actualSize {
			o.emit(map[string]any{
				"type":      "autosize",
				"from":      actualSize,
				"to":        needed,
				"modules":   modules,
				"target_px": o.TargetPx,
			})
			actualSize = needed
		}
	}
	img, err := qrcode.Encode(text, o.RL, actualSize)
	if err != nil {
		return nil, fmt.Errorf("cannot create QR (content too large for level %v): %w", o.RL, err)
	}
	return [][]byte{img}, nil
}

// wifiEscape escapes the characters that are special in the WIFI: QR format.
func wifiEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', ';', ',', ':', '"':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// wifiString builds the standard Wi-Fi join payload understood by the
// Android and iOS cameras: WIFI:T:<WPA|WEP|nopass>;S:<ssid>;P:<pass>;H:true;;
func wifiString(ssid, pass, sec string, hidden bool) (string, error) {
	if ssid == "" {
		return "", errors.New("Wi-Fi name (SSID) is empty")
	}
	switch strings.ToUpper(strings.TrimSpace(sec)) {
	case "", "WPA", "WPA2", "WPA3":
		sec = "WPA"
	case "WEP":
		sec = "WEP"
	case "NOPASS", "NONE", "OPEN":
		sec = "nopass"
	default:
		return "", fmt.Errorf("unknown security type %q (use WPA, WEP or nopass)", sec)
	}
	if sec != "nopass" && pass == "" {
		return "", errors.New("password is empty (choose \"No password\" for an open network)")
	}
	var b strings.Builder
	b.WriteString("WIFI:T:" + sec + ";S:" + wifiEscape(ssid) + ";")
	if sec != "nopass" {
		b.WriteString("P:" + wifiEscape(pass) + ";")
	}
	if hidden {
		b.WriteString("H:true;")
	}
	b.WriteString(";")
	return b.String(), nil
}

func generate(mode, text, name string, data []byte, o opts) ([][]byte, error) {
	encode := func(s string) ([]byte, error) {
		b, err := qrcode.Encode(s, o.RL, o.Size)
		if err != nil {
			return nil, fmt.Errorf("cannot create QR (content too large?): %w", err)
		}
		return b, nil
	}
	switch mode {
	case "text":
		if text == "" {
			return nil, errors.New("content is empty")
		}
		return oneWithAutoSize(text, o)
	case "url":
		u, err := normalizeURL(text)
		if err != nil {
			return nil, err
		}
		return oneWithAutoSize(u, o)
	case "wifi":
		w, err := wifiString(o.WifiSSID, o.WifiPass, o.WifiSec, o.WifiHidden)
		if err != nil {
			return nil, err
		}
		return oneWithAutoSize(w, o)
	case "image":
		if len(data) == 0 {
			return nil, errors.New("no image provided")
		}
		if o.Fit {
			uri, err := fitImage(data, maxDirectLen)
			if err != nil {
				return nil, err
			}
			return oneWithAutoSize(uri, o)
		}
		fallthrough
	case "file":
		if len(data) == 0 {
			return nil, errors.New("no file provided")
		}
		switch o.Format {
		case "pixel":
			return pixelEncode(name, data, o)
		case "grid":
			return gridEncode(name, data, o)
		}
		parts, err := chunkStrings(name, data, o)
		if err != nil {
			return nil, err
		}
		imgs := make([][]byte, 0, len(parts))
		for _, p := range parts {
			b, err := encode(p)
			if err != nil {
				return nil, err
			}
			imgs = append(imgs, b)
			o.emit(map[string]any{"type": "progress", "done": len(imgs), "total": len(parts)})
		}
		return imgs, nil
	}
	return nil, fmt.Errorf("unsupported mode: %q", mode)
}

// ---------- Decoding ----------

func readItem(r io.Reader) (item, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return item{}, fmt.Errorf("cannot read image: %w", err)
	}
	if it, ok := tryPixel(img); ok {
		return it, nil
	}
	if it, ok := tryGrid(img); ok {
		return it, nil
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return item{}, err
	}
	hints := map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true}
	res, err := gzqr.NewQRCodeReader().Decode(bmp, hints)
	if err != nil {
		return item{}, fmt.Errorf("not a pixpack image (pixel/grid) and cannot decode QR: %w", err)
	}
	return item{Text: res.GetText(), Kind: "qr"}, nil
}

type decoded struct {
	Name   string
	Data   []byte
	IsText bool
	Format string
	Images int
	Algo   string
	Packed int
}

type item struct {
	Text       string
	Grid       bool
	Idx, Total int
	CRC        uint32
	Data       []byte
	Kind       string
}

func assemble(items []item) (*decoded, error) {
	d, err := assembleCore(items)
	if err != nil {
		return nil, err
	}
	d.Images = len(items)
	d.Format = "qr"
	for _, it := range items {
		if it.Kind != "" {
			d.Format = it.Kind
			break
		}
	}
	return d, nil
}

func assembleCore(texts []item) (*decoded, error) {
	if len(texts) == 0 {
		return nil, errors.New("no images provided")
	}
	type group struct {
		total  int
		parts  map[int]string
		bparts map[int][]byte
	}
	groups := map[uint32]*group{}
	getGroup := func(crc uint32, total int) *group {
		g := groups[crc]
		if g == nil {
			g = &group{total: total, parts: map[int]string{}, bparts: map[int][]byte{}}
			groups[crc] = g
		}
		return g
	}
	var plain []string
	for _, it := range texts {
		if it.Grid {
			getGroup(it.CRC, it.Total).bparts[it.Idx] = it.Data
			continue
		}
		t := it.Text
		if len(t) >= headerLen && strings.HasPrefix(t, "QRG1") {
			total, e1 := strconv.ParseUint(t[4:8], 16, 32)
			idx, e2 := strconv.ParseUint(t[8:12], 16, 32)
			crc, e3 := strconv.ParseUint(t[12:20], 16, 32)
			if e1 == nil && e2 == nil && e3 == nil && total >= 1 && idx >= 1 && idx <= total {
				getGroup(uint32(crc), int(total)).parts[int(idx)] = t[headerLen:]
				continue
			}
		}
		plain = append(plain, t)
	}
	if len(groups) > 1 {
		return nil, errors.New("images belong to different files, decode each file separately")
	}
	for crc, g := range groups {
		var comp []byte
		if len(g.bparts) > 0 {
			for i := 1; i <= g.total; i++ {
				p, ok := g.bparts[i]
				if !ok {
					return nil, fmt.Errorf("missing image %d/%d", i, g.total)
				}
				comp = append(comp, p...)
			}
		} else {
			var sb strings.Builder
			for i := 1; i <= g.total; i++ {
				p, ok := g.parts[i]
				if !ok {
					return nil, fmt.Errorf("missing QR code %d/%d", i, g.total)
				}
				sb.WriteString(p)
			}
			var err error
			comp, err = b45dec(sb.String())
			if err != nil {
				return nil, err
			}
		}
		if crc32.ChecksumIEEE(comp) != crc {
			return nil, errors.New("checksum mismatch, data corrupted")
		}
		name, data, algo, err := unpack(comp)
		if err != nil {
			return nil, err
		}
		return &decoded{Name: name, Data: data, Algo: algo, Packed: len(comp)}, nil
	}

	t := strings.Join(plain, "\n")
	if strings.HasPrefix(t, "data:") {
		if i := strings.Index(t, ";base64,"); i > 5 {
			mt := t[5:i]
			if raw, err := base64.StdEncoding.DecodeString(t[i+8:]); err == nil {
				ext := ".bin"
				if mt == "image/jpeg" {
					ext = ".jpg"
				} else if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
					ext = exts[0]
				}
				return &decoded{Name: "decoded" + ext, Data: raw}, nil
			}
		}
	}
	return &decoded{IsText: true, Data: []byte(t)}, nil
}

func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	if n == "" || n == "." || n == ".." || n == "/" {
		return "output.bin"
	}
	return n
}

// ---------- main / CLI ----------

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", ":8090", "listen address")
		maxmb := fs.Int64("maxmb", 0, "upload limit (MB), 0 = unlimited")
		fs.Parse(os.Args[2:])
		serverMaxBytes = *maxmb << 20
		serve(*addr)
	case "decode":
		runDecode(os.Args[2:])
	default:
		runEncode()
	}
}

func usage() {
	fmt.Println(`Usage:
  pixpack <text|url|wifi|image|file> <content|path|ssid> [-o out.png] [-s 768] [-l M] [-c 900] [-fit]
                [-fmt pixel|grid|qr] [-depth 16 -ch 4 -px 512] [-bits 2 -cell 3 -grid 256]
                [-target-px 8] [-no-auto]
  pixpack wifi <ssid> [-pass <password>|-pass -] [-sec WPA|WEP|nopass] [-hidden] [-o wifi.png]
  pixpack decode [-o dir] img1.png img2.png ...
  pixpack serve [-addr :8090]

Auto-size (text/url/image -fit):
  If -s is too small for the chosen -l level, the image is grown so
  every QR module is at least -target-px pixels (default 8).
  Use -no-auto to disable and keep -s exactly.`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

func runEncode() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(1)
	}
	mode, input := os.Args[1], os.Args[2]
	fs := flag.NewFlagSet("pixpack", flag.ExitOnError)
	out := fs.String("o", "out.png", "output PNG file")
	size := fs.Int("s", 768, "image size (px)")
	level := fs.String("l", "M", "error correction level: L, M, Q, H")
	chunk := fs.Int("c", 900, "data characters per QR when splitting")
	fit := fs.Bool("fit", false, "image: shrink + JPEG compress to fit 1 QR")
	max := fs.Int("max", maxChunks, "max QR/images")
	format := fs.String("fmt", "pixel", "format for image/file: pixel (densest), grid, qr")
	depth := fs.Int("depth", 16, "pixel: 8 or 16 bits/channel")
	chn := fs.Int("ch", 4, "pixel: 3 (RGB) or 4 (RGBA)")
	px := fs.Int("px", 512, "pixel: pixels per image side")
	bits := fs.Int("bits", 2, "grid: bits/color channel (1-8), higher = denser but less robust")
	cellPx := fs.Int("cell", 3, "grid: pixels per cell")
	gridW := fs.Int("grid", 256, "grid: cells per side")
	targetPx := fs.Int("target-px", 8, "auto-size: min pixels per QR module (4-16)")
	noAuto := fs.Bool("no-auto", false, "disable auto-size (use -s exactly, may be unscannable)")
	wpass := fs.String("pass", "", "wifi: password (use - to read it from stdin)")
	wsec := fs.String("sec", "WPA", "wifi: security WPA, WEP or nopass")
	whid := fs.Bool("hidden", false, "wifi: hidden network")
	fs.Parse(os.Args[3:])

	var text, name string
	var data []byte
	var err error
	switch mode {
	case "text", "url":
		text = input
		if input == "-" {
			var b []byte
			b, err = io.ReadAll(os.Stdin)
			text = strings.TrimRight(string(b), "\r\n")
		}
	case "wifi":
		if *wpass == "-" {
			var b []byte
			b, err = io.ReadAll(os.Stdin)
			*wpass = strings.TrimRight(string(b), "\r\n")
		}
	case "image", "file":
		name = filepath.Base(input)
		data, err = os.ReadFile(input)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fatal(err)
	}

	o := opts{RL: parseLevel(*level), Size: *size, Chunk: *chunk, MaxChunks: *max, Fit: *fit,
		Format: *format, Bits: clampInt(*bits, 1, 8), Cell: clampInt(*cellPx, 1, 16), GridW: clampInt(*gridW, 16, 2048),
		Depth: normDepth(*depth), Channels: normCh(*chn), PixSide: clampInt(*px, 16, 4096),
		TargetPx: clampInt(*targetPx, 4, 16), NoAuto: *noAuto,
		WifiSSID: input, WifiPass: *wpass, WifiSec: *wsec, WifiHidden: *whid}
	o.Report = cliReport()
	imgs, err := generate(mode, text, name, data, o)
	if err != nil {
		fatal(err)
	}
	base := strings.TrimSuffix(*out, filepath.Ext(*out))
	for i, img := range imgs {
		fn := *out
		if len(imgs) > 1 {
			fn = fmt.Sprintf("%s_%03d.png", base, i+1)
		}
		if err := os.WriteFile(fn, img, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("Created:", fn)
	}
	if len(imgs) > 1 {
		fmt.Printf("Split into %d images. Restore with: pixpack decode -o out/ %s_*.png\n", len(imgs), base)
	}
}

func runDecode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	out := fs.String("o", ".", "output directory")
	fs.Parse(args)
	paths := fs.Args()
	if len(paths) == 0 {
		usage()
		os.Exit(1)
	}
	var texts []item
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			fatal(err)
		}
		t, err := readItem(f)
		f.Close()
		if err != nil {
			fatal(fmt.Errorf("%s: %w", p, err))
		}
		texts = append(texts, t)
	}
	d, err := assemble(texts)
	if err != nil {
		fatal(err)
	}
	if d.IsText {
		fmt.Println(string(d.Data))
		return
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	dest := filepath.Join(*out, safeName(d.Name))
	if err := os.WriteFile(dest, d.Data, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("Restored: %s (%d bytes)\n", dest, len(d.Data))
}

// ---------- Web server ----------

func clampSize(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 128 {
		return 768
	}
	if n > 2048 {
		return 2048
	}
	return n
}

func atoiDef(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func optsFrom(get func(string) string) opts {
	o := opts{
		RL:        parseLevel(get("level")),
		Size:      clampSize(get("size")),
		Chunk:     900,
		MaxChunks: maxChunks,
		Fit:       get("fit") == "1",
		Format:    get("format"),
		Bits:      clampInt(atoiDef(get("bits"), 2), 1, 8),
		Cell:      clampInt(atoiDef(get("cell"), 3), 1, 16),
		GridW:     clampInt(atoiDef(get("gridw"), 256), 16, 2048),
		Depth:     normDepth(atoiDef(get("depth"), 16)),
		Channels:  normCh(atoiDef(get("ch"), 4)),
		PixSide:   clampInt(atoiDef(get("px"), 512), 16, 4096),
		TargetPx:  clampInt(atoiDef(get("targetpx"), 8), 4, 16),
		NoAuto:    get("noauto") == "1",

		WifiSSID:   get("ssid"),
		WifiPass:   get("wpass"),
		WifiSec:    get("wsec"),
		WifiHidden: get("whidden") == "1",
	}
	if n, err := strconv.Atoi(get("chunk")); err == nil && n >= 100 && n <= 2000 {
		o.Chunk = n
	}
	return o
}

// extMime covers common media types that the stdlib mime table (which
// depends on the OS) may not know, so the browser can preview them.
var extMime = map[string]string{
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".ogv": "video/ogg",
	".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".oga": "audio/ogg",
	".opus": "audio/ogg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac",
	".avif": "image/avif", ".bmp": "image/bmp",
}

func serve(addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, indexHTML)
	})

	mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mode := q.Get("mode")
		if mode == "" {
			mode = "text"
		}
		if mode != "text" && mode != "url" {
			http.Error(w, "GET /qr only supports mode=text or url", http.StatusBadRequest)
			return
		}
		imgs, err := generate(mode, q.Get("text"), "", nil, optsFrom(q.Get))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(imgs[0])
	})

	mux.HandleFunc("/api/encode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		if r.Method != http.MethodPost {
			fail(http.StatusMethodNotAllowed, "only POST supported")
			return
		}
		limitBody(w, r)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			fail(http.StatusBadRequest, "invalid data or file exceeds upload limit")
			return
		}
		mode := r.FormValue("mode")
		var name string
		var data []byte
		if mode == "file" || mode == "image" {
			f, hdr, err := r.FormFile("file")
			if err != nil {
				fail(http.StatusBadRequest, "no file selected")
				return
			}
			defer f.Close()
			name = filepath.Base(hdr.Filename)
			data, err = io.ReadAll(f)
			if err != nil {
				fail(http.StatusBadRequest, "cannot read file")
				return
			}
		}
		oo := optsFrom(r.FormValue)
		if r.FormValue("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			fl, _ := w.(http.Flusher)
			var emu sync.Mutex
			enc := json.NewEncoder(w)
			emit := func(e map[string]any) {
				emu.Lock()
				enc.Encode(e)
				if fl != nil {
					fl.Flush()
				}
				emu.Unlock()
			}
			oo.Report = emit
			start := time.Now()
			emit(map[string]any{"type": "stage", "msg": "Server is processing"})
			imgs, err := generate(mode, r.FormValue("text"), name, data, oo)
			if err != nil {
				emit(map[string]any{"type": "error", "error": err.Error()})
				return
			}
			eff := oo.Format
			if mode == "text" || mode == "url" || mode == "wifi" || (mode == "image" && oo.Fit) || eff == "" {
				eff = "qr"
			}
			inBytes := len(data)
			if mode == "text" || mode == "url" {
				inBytes = len(r.FormValue("text"))
			} else if mode == "wifi" {
				inBytes = len(oo.WifiSSID) + len(oo.WifiPass)
			}
			uris := make([]string, len(imgs))
			outBytes := 0
			for i, b := range imgs {
				outBytes += len(b)
				uris[i] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
			}
			emit(map[string]any{"type": "done", "images": uris, "count": len(uris), "outBytes": outBytes,
				"inBytes": inBytes, "format": eff, "ms": time.Since(start).Milliseconds(),
				"precompressed": looksPrecompressed(name, data)})
			return
		}
		imgs, err := generate(mode, r.FormValue("text"), name, data, oo)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if r.FormValue("zip") == "1" {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="pixpack.zip"`)
			zw := zip.NewWriter(w)
			for i, b := range imgs {
				f, err := zw.Create(fmt.Sprintf("img_%03d.png", i+1))
				if err != nil {
					return
				}
				f.Write(b)
			}
			zw.Close()
			return
		}
		uris := make([]string, len(imgs))
		for i, b := range imgs {
			uris[i] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
		}
		json.NewEncoder(w).Encode(map[string]any{"images": uris, "count": len(uris)})
	})

	mux.HandleFunc("/api/decode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "only POST supported", http.StatusMethodNotAllowed)
			return
		}
		limitBody(w, r)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid data or too large", http.StatusBadRequest)
			return
		}
		fhs := r.MultipartForm.File["files"]
		if len(fhs) == 0 {
			http.Error(w, "no QR images selected", http.StatusBadRequest)
			return
		}
		start := time.Now()
		var texts []item
		for _, fh := range fhs {
			f, err := fh.Open()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			t, err := readItem(f)
			f.Close()
			if err != nil {
				http.Error(w, fmt.Sprintf("%s: %v", fh.Filename, err), http.StatusBadRequest)
				return
			}
			texts = append(texts, t)
		}
		d, err := assemble(texts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, _ := json.Marshal(map[string]any{"format": d.Format, "images": d.Images, "algo": d.Algo,
			"packed": d.Packed, "name": safeName(d.Name), "size": len(d.Data),
			"ms": time.Since(start).Milliseconds(), "text": d.IsText})
		w.Header().Set("X-Pp-Info", url.PathEscape(string(info)))
		if d.IsText {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(d.Data)
			return
		}
		name := safeName(d.Name)
		ext := strings.ToLower(filepath.Ext(name))
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = extMime[ext]
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Write(d.Data)
	})

	log.Printf("pixpack server running at: http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

const indexHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PixPack</title>
<style>
body{font-family:system-ui,sans-serif;max-width:680px;margin:0 auto;padding:16px;line-height:1.4}
h2{margin-top:28px}
label{display:block;margin-top:12px;font-weight:600}
select,textarea,input,button{width:100%;box-sizing:border-box;padding:10px;font-size:16px;margin-top:4px}
input[type=checkbox]{width:auto;margin-right:6px}
button{background:#2563eb;color:#fff;border:0;border-radius:8px;margin-top:16px;cursor:pointer}
button.sec{background:#475569}
.row{display:flex;gap:8px}.row>div{flex:1}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:8px;margin-top:12px}
.grid img{width:100%;border:1px solid #ccc}
.err{color:#b91c1c;margin-top:12px;white-space:pre-wrap}
pre{white-space:pre-wrap;word-break:break-all;background:#f1f5f9;padding:8px;border-radius:6px}
small{color:#64748b;display:block;margin-top:4px}
.card{border:1px solid #cbd5e1;border-radius:10px;padding:12px;margin-top:12px}
.card h3{margin:0 0 8px;font-size:15px}
.bar{height:12px;background:#e2e8f0;border-radius:6px;overflow:hidden;margin-top:8px}
.fill{height:100%;width:0;background:#2563eb;transition:width .15s}
table{width:100%;border-collapse:collapse;font-size:14px}
td,th{padding:5px 6px;text-align:left;border-bottom:1px solid #e2e8f0;vertical-align:middle}
tr.best td{font-weight:600}
.mini{height:8px;background:#e2e8f0;border-radius:4px;overflow:hidden;margin-bottom:2px}
.mini>div{height:100%;background:#94a3b8}
tr.best .mini>div{background:#16a34a}
.kv{display:grid;grid-template-columns:auto 1fr;gap:4px 12px;font-size:14px;margin:8px 0}
.kv>b{color:#0f172a;font-weight:600}
.kv>span{color:#334155}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;font-weight:600;vertical-align:middle;margin-left:6px}
.badge.auto{background:#fef3c7;color:#92400e;border:1px solid #f59e0b}
.badge.manual{background:#e0e7ff;color:#3730a3;border:1px solid #818cf8}
.badge.ok{background:#dcfce7;color:#166534;border:1px solid #22c55e}
.arrow{color:#64748b;font-weight:400;margin:0 4px}
.hint{color:#64748b;font-size:12px;margin-top:6px}
</style></head><body>
<h2>Encode → Image</h2>
<label>Mode</label>
<select id="mode">
<option value="text">Text</option>
<option value="url">URL / Link</option>
<option value="wifi">Wi-Fi network</option>
<option value="image">Image</option>
<option value="file">Any binary file</option>
</select>
<div id="textBox"><label>Content</label><textarea id="text" rows="4"></textarea></div>
<div id="wifiBox" style="display:none">
<label>Wi-Fi name (SSID)</label><input id="ssid" type="text" autocomplete="off">
<label>Password</label><input id="wpass" type="password" autocomplete="off">
<label style="font-weight:400"><input type="checkbox" id="wshow"> Show password</label>
<div class="row">
<div><label>Security</label><select id="wsec"><option value="WPA">WPA / WPA2 / WPA3</option><option value="WEP">WEP</option><option value="nopass">No password</option></select></div>
<div><label style="margin-top:24px"><input type="checkbox" id="whidden"> Hidden network</label></div>
</div>
<small>Scan with the phone camera to join. The password is sent to this server only to draw the QR and is not saved.</small>
</div>
<div id="fileBox" style="display:none"><label>Choose file</label><input type="file" id="file"><small id="finfo"></small><div id="fpreview"></div></div>
<div id="fitBox" style="display:none"><label><input type="checkbox" id="fit"> Shrink + JPEG compress to fit 1 QR (lossy)</label>
<small>Unchecked: keep original image, auto-split into multiple images.</small></div>
<div id="fmtBox" style="display:none">
<label>Output format</label>
<select id="format"><option value="pixel">Pixel PNG (densest, pixpack-only)</option><option value="grid">Color grid (pixpack-only)</option><option value="qr">QR codes (scannable by phone)</option></select>
<div id="pixBox"><div class="row">
<div><label>Bits / channel</label><select id="depth"><option>16</option><option>8</option></select></div>
<div><label>Channels</label><select id="chn"><option value="4">RGBA (4)</option><option value="3">RGB (3)</option></select></div>
<div><label>Pixels / side</label><input id="px" type="number" value="512" min="16" max="4096"></div>
</div></div>
<div id="gridBox" style="display:none"><div class="row">
<div><label>Bits / channel (1-8)</label><input id="bits" type="number" value="2" min="1" max="8"></div>
<div><label>Pixels per cell</label><input id="cell" type="number" value="3" min="1" max="16"></div>
<div><label>Cells per side</label><input id="gridw" type="number" value="256" min="16" max="2048"></div>
</div></div>
</div>
<div class="row">
<div><label>Image size (px)</label><input id="size" type="number" value="768" min="128" max="4096"></div>
<div><label>Error correction</label><select id="level"><option>L</option><option selected>M</option><option>Q</option><option>H</option></select></div>
</div>
<div class="row">
<div><label>Target module px</label><input id="targetpx" type="number" value="8" min="4" max="16"></div>
<div><label style="margin-top:24px"><input type="checkbox" id="noauto"> Disable auto-size</label></div>
</div>
<small>Auto-size grows the image if modules would be &lt; target px (QR would be unscannable).</small>
<button id="go">Encode</button>

<div class="card" id="eprog" style="display:none">
<b id="ephase">…</b>
<div class="bar"><div class="fill" id="efill"></div></div>
<small id="edetail"></small>
</div>
<div class="err" id="err"></div>
<div class="card" id="stats" style="display:none"></div>
<div class="card" id="algos" style="display:none"></div>
<div id="out"></div>

<h2>Decode image(s) → original file</h2>
<label>Select image(s) — multiple allowed</label>
<input type="file" id="dfiles" accept="image/*" multiple>
<small id="dfinfo"></small>
<button id="dgo" class="sec">Decode</button>
<div class="card" id="dprog" style="display:none">
<b id="dphase">…</b>
<div class="bar"><div class="fill" id="dfill"></div></div>
<small id="ddetail"></small>
</div>
<div class="err" id="derr"></div>
<div class="card" id="dstats" style="display:none"></div>
<div id="dout"></div>

<script>
var $=function(i){return document.getElementById(i)};
function fmt(n){if(n<1024)return n+" B";if(n<1048576)return(n/1024).toFixed(1)+" KB";if(n<1073741824)return(n/1048576).toFixed(2)+" MB";return(n/1073741824).toFixed(2)+" GB";}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[c];});}
function sync(){
  var m=$("mode").value,f=(m==="file"||m==="image");
  $("fileBox").style.display=f?"":"none";
  var w=(m==="wifi");
  $("textBox").style.display=(f||w)?"none":"";
  $("wifiBox").style.display=w?"":"none";
  $("wpass").disabled=(w&&$("wsec").value==="nopass");
  $("fitBox").style.display=(m==="image")?"":"none";
  $("fmtBox").style.display=f?"":"none";
  $("gridBox").style.display=(f&&$("format").value==="grid")?"":"none";
  $("pixBox").style.display=(f&&$("format").value==="pixel")?"":"none";
}
$("mode").onchange=sync;$("format").onchange=sync;$("wsec").onchange=sync;
$("wshow").onchange=function(){$("wpass").type=this.checked?"text":"password";};sync();
$("file").onchange=function(){var f=this.files[0];$("finfo").textContent=f?(f.name+" — "+fmt(f.size)):"";preview(f,f?f.name:"",$("fpreview"));};
$("dfiles").onchange=function(){var t=0;for(var i=0;i<this.files.length;i++)t+=this.files[i].size;$("dfinfo").textContent=this.files.length?(this.files.length+" image(s) — "+fmt(t)):"";};
function save(blob,name){var a=document.createElement("a");a.href=URL.createObjectURL(blob);a.download=name;document.body.appendChild(a);a.click();a.remove();}
function build(){
  var fd=new FormData();
  fd.append("mode",$("mode").value);fd.append("text",$("text").value);
  fd.append("ssid",$("ssid").value);fd.append("wpass",$("wpass").value);fd.append("wsec",$("wsec").value);fd.append("whidden",$("whidden").checked?"1":"0");
  fd.append("size",$("size").value);fd.append("level",$("level").value);
  fd.append("fit",$("fit").checked?"1":"0");
  fd.append("format",$("format").value);fd.append("bits",$("bits").value);
  fd.append("cell",$("cell").value);fd.append("gridw",$("gridw").value);
  fd.append("depth",$("depth").value);fd.append("ch",$("chn").value);fd.append("px",$("px").value);
  fd.append("targetpx",$("targetpx").value);fd.append("noauto",$("noauto").checked?"1":"0");
  if($("file").files[0])fd.append("file",$("file").files[0]);
  return fd;
}
function showProg(id,on){$(id).style.display=on?"":"none";}
function setBar(pre,p,title,detail){
  $(pre+"fill").style.width=Math.max(0,Math.min(100,p))+"%";
  $(pre+"phase").textContent=title;
  $(pre+"detail").textContent=detail||"";
}
function row(k,v){return "<tr><td>"+k+"</td><td>"+v+"</td></tr>";}
var EXTMIME={mp4:"video/mp4",m4v:"video/mp4",webm:"video/webm",ogv:"video/ogg",mov:"video/quicktime",mkv:"video/x-matroska",mp3:"audio/mpeg",wav:"audio/wav",ogg:"audio/ogg",oga:"audio/ogg",opus:"audio/ogg",m4a:"audio/mp4",aac:"audio/aac",flac:"audio/flac",png:"image/png",jpg:"image/jpeg",jpeg:"image/jpeg",gif:"image/gif",webp:"image/webp",svg:"image/svg+xml",bmp:"image/bmp",avif:"image/avif",pdf:"application/pdf"};
var TEXTEXT=["txt","md","json","csv","tsv","log","xml","html","htm","css","js","ts","go","py","c","h","cpp","java","rs","sh","yml","yaml","toml","ini","sql","conf"];
function mimeOf(name,fallback){
  var e=(name||"").split(".").pop().toLowerCase();
  if(EXTMIME[e])return EXTMIME[e];
  if(TEXTEXT.indexOf(e)>=0)return "text/plain";
  return (fallback||"").split(";")[0];
}
// preview(blob,name,box,fallbackType): inline preview of video, audio, image, PDF or text.
// Everything stays in the browser (blob URLs); text is shown with textContent only.
function preview(blob,name,box,fallback){
  box.innerHTML="";
  if(!blob)return;
  var mt=mimeOf(name,fallback||blob.type),k=mt.split("/")[0],el;
  var note=function(t){var s=document.createElement("small");s.textContent=t;box.appendChild(s);};
  if(k==="video"||k==="audio"){
    el=document.createElement(k);el.controls=true;el.preload="metadata";
    el.style.cssText="width:100%;margin-top:8px;max-height:70vh;background:#000";
    el.onerror=function(){note("This browser cannot play this file ("+mt+"). Download it instead.");};
    el.src=URL.createObjectURL(new Blob([blob],{type:mt}));
    box.appendChild(el);
  }else if(k==="image"){
    el=new Image();el.style.cssText="max-width:100%;margin-top:8px";
    el.onerror=function(){el.remove();note("Cannot preview this image.");};
    el.src=URL.createObjectURL(new Blob([blob],{type:mt}));
    box.appendChild(el);
  }else if(mt==="application/pdf"){
    el=document.createElement("iframe");el.style.cssText="width:100%;height:70vh;border:1px solid #cbd5e1;margin-top:8px";
    el.src=URL.createObjectURL(new Blob([blob],{type:mt}));
    box.appendChild(el);
  }else if(mt==="text/plain"){
    var lim=65536,fr=new FileReader();
    fr.onload=function(){
      var p=document.createElement("pre");p.style.maxHeight="50vh";p.style.overflow="auto";
      p.textContent=fr.result;box.appendChild(p);
      if(blob.size>lim)note("Preview shows the first "+fmt(lim)+" of "+fmt(blob.size)+".");
    };
    fr.readAsText(blob.slice(0,lim));
  }
}

function skipLabel(r){
  switch(r.reason){
    case "too_large": return "skipped (file too large)";
    case "incompressible": return "skipped (incompressible data)";
    case "use_parallel": return "skipped (using lzw-parallel instead)";
    case "use_single": return "skipped (using lzw instead)";
    case "precompressed": return "skipped (already compressed content)";
    case "not_dna": return "skipped (not DNA)";
  }
  return "skipped";
}

function render(d,rows,pack,autoInfo){
  var oldQD=document.getElementById("qrDetail");
  if(oldQD)oldQD.parentNode.removeChild(oldQD);
  var h="";
  if(d.precompressed){
    h+="<div class=\"notice\" style=\"background:#fef3c7;border:1px solid #f59e0b;color:#78350f;padding:10px;border-radius:8px;margin-bottom:12px\">";
    h+="⚠️ <b>Already-compressed file detected</b> (video/audio/image/archive/office). ";
    h+="Slow algorithms (xz, brotli, CMX1) were skipped — they only save &lt;10% but cost minutes of CPU. ";
    h+="Only fast algorithms (gzip, zstd, lzw, lzh) were tried.";
    h+="</div>";
  }
  h+="<h3>Result</h3><table>";
  h+=row("Format",esc(d.format));
  h+=row("Original size",fmt(d.inBytes));
  if(pack){
    var sv=pack.orig-pack.packed;
    var pc=pack.orig?(pack.packed/pack.orig*100).toFixed(1)+"% of original":"";
    h+=row("After compression",fmt(pack.packed)+" ("+pc+(pack.orig?", "+(sv>=0?"saved ":"increased ")+fmt(Math.abs(sv)):"")+")");
    h+=row("Chosen algorithm","<b>"+esc(pack.algo)+"</b>");
  }
  h+=row("Image count",d.count);
  h+=row("Total image size",fmt(d.outBytes)+(d.inBytes?" ("+(d.outBytes/d.inBytes).toFixed(2)+"× original)":""));
  h+=row("Processing time",(d.ms/1000).toFixed(2)+" s");
  h+="</table>";
  $("stats").innerHTML=h;$("stats").style.display="";

  // ---- QR detail card ----
  // Only render QR detail card for QR format with real metadata.
  var showQR = (d.format === "qr") && (d.qr_modules > 0 || !!autoInfo);
  if(showQR){
    var mods = d.qr_modules || (autoInfo && autoInfo.modules);
    var ver  = d.qr_version || (autoInfo && autoInfo.version);
    var px   = d.qr_px;
    var mpx  = d.qr_module_px || (autoInfo && autoInfo.module_px);
    var tgt  = d.qr_target_px || (autoInfo && autoInfo.target_px);
    var lvl  = d.qr_level;
    var wasAuto = !!(autoInfo || (px && tgt && mpx && mpx + 0.01 >= tgt && px > (autoInfo?autoInfo.from:0)));
    // more reliable: autoInfo present means auto-adjust happened
    wasAuto = !!autoInfo;

    var q="<h3>QR detail"+(wasAuto?'<span class="badge auto">auto-sized</span>':'<span class="badge manual">manual size</span>')+"</h3>";
    q+='<div class="kv">';
    if(ver) q+="<b>Version</b><span>v"+ver+"</span>";
    if(mods) q+="<b>Modules</b><span>"+mods+" × "+mods+"</span>";
    if(px) q+="<b>Rendered size</b><span>"+px+" × "+px+" px</span>";
    if(mpx) q+="<b>Module px</b><span>"+mpx.toFixed(2)+" px/module</span>";
    if(tgt) q+="<b>Target px</b><span>≥ "+tgt+" px/module"+(mpx&&mpx+0.01<tgt?' <span style="color:#b91c1c">(below target)</span>':' <span class="badge ok">ok</span>')+"</span>";
    if(lvl) q+="<b>EC level</b><span>"+lvl+"</span>";
    q+="</div>";

    if(wasAuto){
      q+='<div class="hint">Auto-size changed the image from <b>'+autoInfo.from+'px</b> <span class="arrow">→</span> <b>'+autoInfo.to+'px</b> '+
         'so each module is at least '+autoInfo.target_px+'px (otherwise phones may fail to scan).</div>';
    }else{
      q+='<div class="hint">Auto-size did not trigger — the requested size already gives ≥'+ (tgt||8) +'px per module.</div>';
    }
    q+='<div style="margin-top:8px"><button class="sec" style="width:auto;padding:6px 12px;font-size:13px" onclick="document.getElementById(\'qrDetail\').style.display=\'none\'">Hide</button></div>';
    var card=document.createElement("div");card.className="card";card.id="qrDetail";
    card.innerHTML=q;
    $("stats").parentNode.insertBefore(card,$("stats").nextSibling);
  }

  if(rows.length){
    var rs=rows.slice().sort(function(a,b){return (a.size||1e18)-(b.size||1e18);});
    var t="<h3>Algorithm comparison</h3><table><tr><th>Algorithm</th><th>Size</th><th>vs original</th><th>Time</th></tr>";
    rs.forEach(function(r){
      if(r.skipped){t+="<tr><td>"+esc(r.name)+"</td><td colspan=\"3\">"+skipLabel(r)+"</td></tr>";return;}
      if(r.failed){t+="<tr><td>"+esc(r.name)+"</td><td colspan=\"3\">failed / not usable</td></tr>";return;}
      var best=pack&&r.name===pack.algo;
      var p=pack&&pack.orig?r.size/pack.orig*100:0;
      t+="<tr"+(best?" class=\"best\"":"")+"><td>"+(best?"★ ":"")+esc(r.name)+"</td><td>"+fmt(r.size)+"</td><td><div class=\"mini\"><div style=\"width:"+Math.min(100,p)+"%\"></div></div>"+p.toFixed(1)+"%</td><td>"+r.ms+" ms</td></tr>";
    });
    t+="</table>";
    $("algos").innerHTML=t;$("algos").style.display="";
  }

  var max=60,g=document.createElement("div");g.className="grid";
  d.images.slice(0,max).forEach(function(u,i){var im=new Image();im.src=u;im.alt="Image "+(i+1);g.appendChild(im);});
  $("out").appendChild(g);
  if(d.count>max){var n=document.createElement("small");n.textContent="Showing first "+max+" of "+d.count+" images. Download ZIP for all.";$("out").appendChild(n);}
  if(d.count===1){
    var a=document.createElement("a");a.href=d.images[0];a.download="out.png";a.textContent="Download image";a.style.display="block";a.style.marginTop="8px";$("out").appendChild(a);
  }else{
    var b=document.createElement("button");b.textContent="Download all "+d.count+" images (ZIP)";
    b.onclick=function(){
      var z=build();z.append("zip","1");
      fetch("/api/encode",{method:"POST",body:z}).then(function(r){return r.blob();}).then(function(bl){save(bl,"pixpack.zip");});
    };
    $("out").appendChild(b);
  }
}

$("go").onclick=function(){
  $("err").textContent="";$("out").innerHTML="";
  $("stats").style.display="none";$("algos").style.display="none";
  var oldQD=document.getElementById("qrDetail");
  if(oldQD)oldQD.parentNode.removeChild(oldQD);
  var fd=build();fd.append("stream","1");
  var xhr=new XMLHttpRequest();xhr.open("POST","/api/encode");
  var pos=0,total=0,doneN=0,rows=[],pack=null,finished=false,autoInfo=null;
  showProg("eprog",true);setBar("e",0,"Uploading…","");
  xhr.upload.onprogress=function(e){if(e.lengthComputable)setBar("e",e.loaded/e.total*100,"Uploading",fmt(e.loaded)+" / "+fmt(e.total));};
  xhr.upload.onload=function(){setBar("e",0,"Server is processing…","");};
  function handle(e){
    if(e.error){$("err").textContent=e.error;showProg("eprog",false);finished=true;return;}
    if(e.type==="stage")setBar("e",0,e.msg,"");
    else if(e.type==="autosize"){
      autoInfo=e;
      setBar("e",5,"Auto-sizing QR",
        "size "+e.from+"px → "+e.to+"px (v"+e.version+", modules="+e.modules+
        ", target "+e.target_px+"px/module)");
    }
    else if(e.type==="packstart"){total=e.total;doneN=0;rows=[];setBar("e",0,"Compressing","0/"+total+" algorithms");}
    else if(e.type==="algo"){
      rows.push(e);
      doneN++;
      var lbl=e.skipped?" (skipped)":(e.failed?" (failed)":"");
      setBar("e",doneN/total*100,"Compressing",doneN+"/"+total+" — "+e.name+lbl);
    }
    else if(e.type==="pack")pack=e;
    else if(e.type==="progress")setBar("e",e.done/e.total*100,"Creating images",e.done+"/"+e.total);
    else if(e.type==="done"){finished=true;setBar("e",100,"Done",(e.ms/1000).toFixed(2)+" s");render(e,rows,pack,autoInfo);}
  }
  function consume(){
    var t=xhr.responseText,nl;
    while((nl=t.indexOf("\n",pos))>=0){
      var line=t.substring(pos,nl);pos=nl+1;
      if(line){try{handle(JSON.parse(line));}catch(x){}}
    }
  }
  xhr.onprogress=consume;
  xhr.onload=function(){
    consume();
    if(!finished){showProg("eprog",false);$("err").textContent=xhr.status!==200?("Error "+xhr.status):"Server finished without result";}
  };
  xhr.onerror=function(){showProg("eprog",false);$("err").textContent="Cannot connect to server";};
  xhr.send(fd);
};

$("dgo").onclick=function(){
  $("derr").textContent="";$("dout").innerHTML="";$("dstats").style.display="none";
  var fs=$("dfiles").files;
  if(!fs.length){$("derr").textContent="No images selected";return;}
  var fd=new FormData();
  for(var i=0;i<fs.length;i++)fd.append("files",fs[i]);
  var xhr=new XMLHttpRequest();xhr.open("POST","/api/decode");xhr.responseType="blob";
  showProg("dprog",true);setBar("d",0,"Uploading…","");
  xhr.upload.onprogress=function(e){if(e.lengthComputable)setBar("d",e.loaded/e.total*100,"Uploading",fmt(e.loaded)+" / "+fmt(e.total));};
  xhr.upload.onload=function(){setBar("d",100,"Server is reading and decompressing…","");};
  xhr.onerror=function(){showProg("dprog",false);$("derr").textContent="Cannot connect to server";};
  xhr.onload=function(){
    showProg("dprog",false);
    if(xhr.status!==200){var fr=new FileReader();fr.onload=function(){$("derr").textContent=fr.result;};fr.readAsText(xhr.response);return;}
    var info={};
    try{info=JSON.parse(decodeURIComponent(xhr.getResponseHeader("X-Pp-Info")||"{}"));}catch(x){}
    var names={pixel:"Pixel PNG",grid:"Color grid",qr:"QR"};
    var h="<h3>Decode result</h3><table>";
    h+=row("Image format",esc(names[info.format]||info.format||"?"));
    h+=row("Images read",info.images);
    if(info.algo)h+=row("Compression","<b>"+esc(info.algo)+"</b>");
    if(info.packed){h+=row("Compressed size",fmt(info.packed));}
    if(!info.text)h+=row("Restored file",esc(info.name)+" ("+fmt(info.size)+")");
    else h+=row("Type","Text ("+fmt(info.size)+")");
    if(info.packed&&info.size)h+=row("Compression ratio",(info.packed/info.size*100).toFixed(1)+"% of original");
    h+=row("Processing time",(info.ms/1000).toFixed(2)+" s");
    h+="</table>";
    $("dstats").innerHTML=h;$("dstats").style.display="";
    var ct=xhr.getResponseHeader("Content-Type")||"";
    var fname=info.name||(info.text?"decoded.txt":"decoded.bin");
    var pv=document.createElement("div");$("dout").appendChild(pv);
    preview(xhr.response,fname,pv,ct);
    var a=document.createElement("a");a.href=URL.createObjectURL(xhr.response);a.download=fname;a.textContent="Download "+fname;a.style.display="block";a.style.marginTop="8px";$("dout").appendChild(a);
  };
  xhr.send(fd);
};
</script></body></html>`

// ======================================================================
// Color grid format
// ======================================================================

const (
	gridHdrLen   = 21
	gridHdrCells = (gridHdrLen * 8) / 3
)

type bitReader struct {
	data []byte
	pos  int
}

func (b *bitReader) read(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		bit := 0
		if idx := b.pos >> 3; idx < len(b.data) {
			bit = int(b.data[idx]>>(7-uint(b.pos&7))) & 1
		}
		v = v<<1 | bit
		b.pos++
	}
	return v
}

type bitWriter struct {
	data []byte
	pos  int
}

func (b *bitWriter) write(v, n int) {
	for i := n - 1; i >= 0; i-- {
		for len(b.data) <= b.pos>>3 {
			b.data = append(b.data, 0)
		}
		if (v>>uint(i))&1 == 1 {
			b.data[b.pos>>3] |= 1 << (7 - uint(b.pos&7))
		}
		b.pos++
	}
}

func gridEncode(name string, data []byte, o opts) ([][]byte, error) {
	comp, err := packWith(name, data, o.Report)
	if err != nil {
		return nil, err
	}
	bpc, w, cell := o.Bits, o.GridW, o.Cell
	capBytes := (w*w - 2 - gridHdrCells) * 3 * bpc / 8
	if capBytes <= 0 {
		return nil, errors.New("grid too small")
	}
	total := (len(comp) + capBytes - 1) / capBytes
	if total > o.MaxChunks || total > 65535 {
		return nil, fmt.Errorf("data too large: need %d images (max %d)", total, o.MaxChunks)
	}
	crc := crc32.ChecksumIEEE(comp)
	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		end := (i + 1) * capBytes
		if end > len(comp) {
			end = len(comp)
		}
		b, err := renderGrid(comp[i*capBytes:end], i+1, total, crc, w, cell, bpc)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
		o.emit(map[string]any{"type": "progress", "done": i + 1, "total": total})
	}
	return out, nil
}

func renderGrid(part []byte, idx, total int, crc uint32, w, cell, bpc int) ([]byte, error) {
	hdr := make([]byte, gridHdrLen)
	copy(hdr, "QCG1")
	hdr[4] = byte(bpc)
	binary.BigEndian.PutUint16(hdr[5:], uint16(idx))
	binary.BigEndian.PutUint16(hdr[7:], uint16(total))
	binary.BigEndian.PutUint32(hdr[9:], crc)
	binary.BigEndian.PutUint32(hdr[13:], uint32(len(part)))
	binary.BigEndian.PutUint32(hdr[17:], crc32.ChecksumIEEE(hdr[:17]))

	bodyCells := (len(part)*8 + 3*bpc - 1) / (3 * bpc)
	n := 2 + gridHdrCells + bodyCells
	h := (n + w - 1) / w
	img := image.NewRGBA(image.Rect(0, 0, w*cell, h*cell))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	set := func(i int, r, g, b uint8) {
		x0, y0 := (i%w)*cell, (i/w)*cell
		for y := y0; y < y0+cell; y++ {
			off := img.PixOffset(x0, y)
			for x := 0; x < cell; x++ {
				img.Pix[off+4*x] = r
				img.Pix[off+4*x+1] = g
				img.Pix[off+4*x+2] = b
				img.Pix[off+4*x+3] = 255
			}
		}
	}
	set(0, 255, 255, 255)
	set(1, 0, 0, 0)

	hr := &bitReader{data: hdr}
	for k := 0; k < gridHdrCells; k++ {
		r, g, b := hr.read(1), hr.read(1), hr.read(1)
		set(2+k, uint8(r*255), uint8(g*255), uint8(b*255))
	}
	levels := 1 << uint(bpc)
	sc := func(v int) uint8 { return uint8(v * 255 / (levels - 1)) }
	br := &bitReader{data: part}
	for k := 0; k < bodyCells; k++ {
		r, g, b := br.read(bpc), br.read(bpc), br.read(bpc)
		set(2+gridHdrCells+k, sc(r), sc(g), sc(b))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func lum(img image.Image, x, y int) int {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(((r + g + b) / 3) >> 8)
}

func tryGrid(img image.Image) (item, bool) {
	b := img.Bounds()
	if b.Dx() < 16 || b.Dy() < 2 || lum(img, b.Min.X, b.Min.Y) < 200 {
		return item{}, false
	}
	cell := 0
	for x := b.Min.X; x < b.Max.X && lum(img, x, b.Min.Y) >= 128; x++ {
		cell++
	}
	if cell < 1 || cell*2 >= b.Dx() {
		return item{}, false
	}
	W, H := b.Dx()/cell, b.Dy()/cell
	sample := func(i int) (int, int, int) {
		cx, cy := i%W, i/W
		if cy >= H {
			return 0, 0, 0
		}
		x0, y0 := b.Min.X+cx*cell, b.Min.Y+cy*cell
		m := cell / 4
		var sr, sg, sb, n uint32
		for y := y0 + m; y < y0+cell-m; y++ {
			for x := x0 + m; x < x0+cell-m; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				sr += r >> 8
				sg += g >> 8
				sb += bl >> 8
				n++
			}
		}
		if n == 0 {
			return 0, 0, 0
		}
		return int(sr / n), int(sg / n), int(sb / n)
	}
	if r, g, bl := sample(1); (r+g+bl)/3 >= 128 {
		return item{}, false
	}

	hw := &bitWriter{}
	for k := 0; k < gridHdrCells; k++ {
		r, g, bl := sample(2 + k)
		for _, v := range [3]int{r, g, bl} {
			bit := 0
			if v >= 128 {
				bit = 1
			}
			hw.write(bit, 1)
		}
	}
	hdr := hw.data
	if len(hdr) < gridHdrLen || string(hdr[:4]) != "QCG1" ||
		crc32.ChecksumIEEE(hdr[:17]) != binary.BigEndian.Uint32(hdr[17:21]) {
		return item{}, false
	}
	bpc := int(hdr[4])
	idx := int(binary.BigEndian.Uint16(hdr[5:]))
	total := int(binary.BigEndian.Uint16(hdr[7:]))
	crc := binary.BigEndian.Uint32(hdr[9:])
	plen := int(binary.BigEndian.Uint32(hdr[13:]))
	if bpc < 1 || bpc > 8 || idx < 1 || idx > total || plen <= 0 {
		return item{}, false
	}
	bodyCells := (plen*8 + 3*bpc - 1) / (3 * bpc)
	if 2+gridHdrCells+bodyCells > W*H {
		return item{}, false
	}
	levels := 1 << uint(bpc)
	bw := &bitWriter{}
	for k := 0; k < bodyCells; k++ {
		r, g, bl := sample(2 + gridHdrCells + k)
		for _, v := range [3]int{r, g, bl} {
			q := (v*(levels-1) + 127) / 255
			if q > levels-1 {
				q = levels - 1
			}
			bw.write(q, bpc)
		}
	}
	if len(bw.data) < plen {
		return item{}, false
	}
	return item{Grid: true, Idx: idx, Total: total, CRC: crc, Data: bw.data[:plen], Kind: "grid"}, true
}

// ======================================================================
// Pixel format
// ======================================================================

const (
	pxHdrLen    = 22
	pxHdrPixels = 8
)

func normDepth(n int) int {
	if n == 8 {
		return 8
	}
	return 16
}

func normCh(n int) int {
	if n == 3 {
		return 3
	}
	return 4
}

func pixelEncode(name string, data []byte, o opts) ([][]byte, error) {
	comp, err := packWith(name, data, o.Report)
	if err != nil {
		return nil, err
	}
	depth, ch, side := o.Depth, o.Channels, o.PixSide
	bpp := ch * depth / 8
	capBytes := (side*side - pxHdrPixels) * bpp
	if capBytes <= 0 {
		return nil, errors.New("image too small")
	}
	total := (len(comp) + capBytes - 1) / capBytes
	if total > o.MaxChunks || total > 65535 {
		return nil, fmt.Errorf("data too large: need %d images (max %d)", total, o.MaxChunks)
	}
	crc := crc32.ChecksumIEEE(comp)
	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		end := (i + 1) * capBytes
		if end > len(comp) {
			end = len(comp)
		}
		b, err := renderPixel(comp[i*capBytes:end], i+1, total, crc, side, depth, ch)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
		o.emit(map[string]any{"type": "progress", "done": i + 1, "total": total})
	}
	return out, nil
}

func renderPixel(part []byte, idx, total int, crc uint32, side, depth, ch int) ([]byte, error) {
	hdr := make([]byte, pxHdrLen)
	copy(hdr, "QPX1")
	hdr[4] = byte(depth)
	hdr[5] = byte(ch)
	binary.BigEndian.PutUint16(hdr[6:], uint16(idx))
	binary.BigEndian.PutUint16(hdr[8:], uint16(total))
	binary.BigEndian.PutUint32(hdr[10:], crc)
	binary.BigEndian.PutUint32(hdr[14:], uint32(len(part)))
	binary.BigEndian.PutUint32(hdr[18:], crc32.ChecksumIEEE(hdr[:18]))

	bpp := ch * depth / 8
	S := 4
	if depth == 16 {
		S = 8
	}
	bodyPixels := (len(part) + bpp - 1) / bpp
	n := pxHdrPixels + bodyPixels
	h := (n + side - 1) / side
	pix := make([]byte, side*h*S)
	for i := range pix {
		pix[i] = 0xFF
	}
	for k := 0; k < pxHdrPixels; k++ {
		for c := 0; c < 3; c++ {
			var v byte
			if bi := k*3 + c; bi < len(hdr) {
				v = hdr[bi]
			}
			off := k*S + c*(S/4)
			for t := 0; t < S/4; t++ {
				pix[off+t] = v
			}
		}
	}
	for k := 0; k < bodyPixels; k++ {
		off := (pxHdrPixels + k) * S
		src := part[k*bpp:]
		if len(src) > bpp {
			src = src[:bpp]
		}
		for t := 0; t < bpp; t++ {
			if t < len(src) {
				pix[off+t] = src[t]
			} else {
				pix[off+t] = 0
			}
		}
	}
	var img image.Image
	if depth == 16 {
		img = &image.NRGBA64{Pix: pix, Stride: side * 8, Rect: image.Rect(0, 0, side, h)}
	} else {
		img = &image.NRGBA{Pix: pix, Stride: side * 4, Rect: image.Rect(0, 0, side, h)}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func tryPixel(img image.Image) (item, bool) {
	var pix []byte
	var stride, S int
	var rect image.Rectangle
	switch v := img.(type) {
	case *image.NRGBA64:
		pix, stride, S, rect = v.Pix, v.Stride, 8, v.Rect
	case *image.RGBA64:
		pix, stride, S, rect = v.Pix, v.Stride, 8, v.Rect
	case *image.NRGBA:
		pix, stride, S, rect = v.Pix, v.Stride, 4, v.Rect
	case *image.RGBA:
		pix, stride, S, rect = v.Pix, v.Stride, 4, v.Rect
	default:
		return item{}, false
	}
	W, H := rect.Dx(), rect.Dy()
	if W < pxHdrPixels || H < 1 {
		return item{}, false
	}
	at := func(i int) int { return (i/W)*stride + (i%W)*S }

	hdr := make([]byte, 0, pxHdrPixels*3)
	for k := 0; k < pxHdrPixels; k++ {
		off := at(k)
		for c := 0; c < 3; c++ {
			hdr = append(hdr, pix[off+c*(S/4)])
		}
	}
	if string(hdr[:4]) != "QPX1" || crc32.ChecksumIEEE(hdr[:18]) != binary.BigEndian.Uint32(hdr[18:22]) {
		return item{}, false
	}
	depth, ch := int(hdr[4]), int(hdr[5])
	idx := int(binary.BigEndian.Uint16(hdr[6:]))
	total := int(binary.BigEndian.Uint16(hdr[8:]))
	crc := binary.BigEndian.Uint32(hdr[10:])
	plen := int(binary.BigEndian.Uint32(hdr[14:]))
	if (depth != 8 && depth != 16) || (ch != 3 && ch != 4) || idx < 1 || idx > total || plen <= 0 {
		return item{}, false
	}
	if (depth == 16) != (S == 8) {
		return item{}, false
	}
	bpp := ch * depth / 8
	bodyPixels := (plen + bpp - 1) / bpp
	if pxHdrPixels+bodyPixels > W*H {
		return item{}, false
	}
	body := make([]byte, 0, bodyPixels*bpp)
	for k := 0; k < bodyPixels; k++ {
		off := at(pxHdrPixels + k)
		body = append(body, pix[off:off+bpp]...)
	}
	return item{Grid: true, Idx: idx, Total: total, CRC: crc, Data: body[:plen], Kind: "pixel"}, true
}

// ======================================================================
// LZW v4
// ======================================================================

const (
	lzwMaxCode   = 4096
	lzwClearCode = 256
	lzwEpochLen  = 2048

	lzwRunBase   = 3000
	lzwRunMinLen = 256
	lzwRunMaxLen = 1 << 24

	lzwHash1Bits = 14
	lzwHash1Size = 1 << lzwHash1Bits
	lzwHash1Mask = lzwHash1Size - 1
	lzwHash2Bits = 12
	lzwHash2Size = 1 << lzwHash2Bits
	lzwHash2Mask = lzwHash2Size - 1

	lzwParallelMinSize = 1 << 20
	lzwParallelChunks  = 8
)

type lzwFastHash struct {
	key1  [lzwHash1Size]uint32
	val1  [lzwHash1Size]int32
	used1 [lzwHash1Size]uint8

	key2  [lzwHash2Size]uint32
	val2  [lzwHash2Size]int32
	used2 [lzwHash2Size]uint8
}

func lzwFastHash32(x uint32) uint32 {
	x *= 0x9E3779B1
	x ^= x >> 15
	x *= 0x85EBCA6B
	x ^= x >> 13
	return x
}

func (h *lzwFastHash) reset() {
	for i := range h.used1 {
		h.used1[i] = 0
	}
	for i := range h.used2 {
		h.used2[i] = 0
	}
}

func (h *lzwFastHash) lookup(key uint32) (int32, bool) {
	h32 := lzwFastHash32(key)
	slot := h32 & lzwHash1Mask
	if h.used1[slot] == 0 {
		return -1, false
	}
	if h.key1[slot] == key {
		return h.val1[slot], true
	}
	slot2 := (h32 >> 14) & lzwHash2Mask
	for h.used2[slot2] != 0 {
		if h.key2[slot2] == key {
			return h.val2[slot2], true
		}
		slot2 = (slot2 + 1) & lzwHash2Mask
	}
	return -1, false
}

func (h *lzwFastHash) insert(key uint32, code int32) {
	h32 := lzwFastHash32(key)
	slot := h32 & lzwHash1Mask
	if h.used1[slot] == 0 {
		h.key1[slot] = key
		h.val1[slot] = code
		h.used1[slot] = 1
		return
	}
	slot2 := (h32 >> 14) & lzwHash2Mask
	for h.used2[slot2] != 0 {
		slot2 = (slot2 + 1) & lzwHash2Mask
	}
	h.key2[slot2] = key
	h.val2[slot2] = code
	h.used2[slot2] = 1
}

type lzwBitWriter struct {
	buf    uint64
	bits   uint
	out    []byte
	outPos int
}

func newLZWBitWriter(capHint int) *lzwBitWriter {
	cap := capHint*2 + 64
	return &lzwBitWriter{out: make([]byte, cap)}
}

func (w *lzwBitWriter) write(code uint32, nbits uint) {
	w.buf = (w.buf << nbits) | uint64(code)
	w.bits += nbits
	for w.bits >= 32 {
		w.bits -= 32
		for len(w.out) < w.outPos+4 {
			w.out = append(w.out, 0, 0, 0, 0)
		}
		binary.BigEndian.PutUint32(w.out[w.outPos:], uint32(w.buf>>w.bits))
		w.outPos += 4
		w.buf &= (1 << w.bits) - 1
	}
}

func (w *lzwBitWriter) flush() []byte {
	for w.bits >= 8 {
		w.bits -= 8
		for len(w.out) <= w.outPos {
			w.out = append(w.out, 0)
		}
		w.out[w.outPos] = byte(w.buf >> w.bits)
		w.outPos++
		w.buf &= (1 << w.bits) - 1
	}
	if w.bits > 0 {
		for len(w.out) <= w.outPos {
			w.out = append(w.out, 0)
		}
		w.out[w.outPos] = byte(w.buf << (8 - w.bits))
		w.outPos++
	}
	return w.out[:w.outPos]
}

type lzwBitReader struct {
	data []byte
	pos  int
	buf  uint64
	bits uint
}

func (r *lzwBitReader) read(nbits uint) (uint32, bool) {
	for r.bits < nbits {
		if r.pos >= len(r.data) {
			return 0, false
		}
		r.buf = (r.buf << 8) | uint64(r.data[r.pos])
		r.pos++
		r.bits += 8
	}
	r.bits -= nbits
	code := uint32(r.buf>>r.bits) & ((1 << nbits) - 1)
	r.buf &= (1 << r.bits) - 1
	return code, true
}

func lzwDetectRun(raw []byte, i int) (byte, int) {
	n := len(raw)
	if i+1 >= n {
		return 0, 0
	}
	b := raw[i]
	j := i + 1
	pattern := uint64(b) * 0x0101010101010101
	for j+8 <= n {
		if binary.LittleEndian.Uint64(raw[j:]) != pattern {
			break
		}
		j += 8
	}
	for j < n && raw[j] == b {
		j++
	}
	runLen := j - i
	if runLen < lzwRunMinLen {
		return 0, 0
	}
	if runLen > lzwRunMaxLen {
		runLen = lzwRunMaxLen
	}
	return b, runLen
}

func lzwEncodeRun(bw *lzwBitWriter, b byte, runLen int, nbits uint) {
	bw.write(uint32(lzwRunBase+int(b)), nbits)
	rem := runLen - lzwRunMinLen
	bw.write(uint32(rem>>16)&0xFFFF, nbits)
	bw.write(uint32(rem>>8)&0xFF, nbits)
	bw.write(uint32(rem)&0xFF, nbits)
}

func lzwDecodeRun(br *lzwBitReader, nbits uint) (int, error) {
	hi, ok1 := br.read(nbits)
	mid, ok2 := br.read(nbits)
	lo, ok3 := br.read(nbits)
	if !ok1 || !ok2 || !ok3 {
		return 0, errors.New("LZW1: run code truncated")
	}
	rem := (int(hi&0xFFFF) << 16) | (int(mid&0xFF) << 8) | int(lo&0xFF)
	return rem + lzwRunMinLen, nil
}

type lzwAdaptiveClear struct {
	ewma       float64
	alpha      float64
	threshold  float64
	minEpochs  int
	epochCount int
	lastRatio  float64
	haveLast   bool
}

func newLZWAdaptiveClear() *lzwAdaptiveClear {
	return &lzwAdaptiveClear{
		alpha:     0.3,
		threshold: 0.92,
		minEpochs: 2,
	}
}

func (a *lzwAdaptiveClear) shouldReset(ratio float64) bool {
	a.epochCount++
	if !a.haveLast {
		a.ewma = ratio
		a.haveLast = true
		a.lastRatio = ratio
		return false
	}
	a.ewma = a.alpha*ratio + (1-a.alpha)*a.ewma
	shouldReset := a.epochCount >= a.minEpochs &&
		ratio < a.ewma*a.threshold &&
		ratio < a.lastRatio
	a.lastRatio = ratio
	return shouldReset
}

func (a *lzwAdaptiveClear) reset() {
	a.ewma = 0
	a.haveLast = false
	a.epochCount = 0
	a.lastRatio = 0
}

func lzwLooksIncompressible(data []byte) bool {
	n := len(data)
	if n == 0 {
		return false
	}
	const sampleSize = 512
	offsets := [4]int{0, n / 4, n / 2, n * 3 / 4}
	var seen [256]bool
	unique := 0
	for _, off := range offsets {
		end := off + sampleSize
		if end > n {
			end = n
		}
		for i := off; i < end; i++ {
			b := data[i]
			if !seen[b] {
				seen[b] = true
				unique++
				if unique > 200 {
					return true
				}
			}
		}
	}
	return false
}

func lzwStoreMode(raw []byte) []byte {
	out := make([]byte, 0, 5+len(raw)+10)
	out = append(out, 'L', 'Z', 'W', 'S')
	var tmp [binary.MaxVarintLen64]byte
	out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))]...)
	out = append(out, raw...)
	return out
}

func lzwShouldStore(rawLen int, compLen int) bool {
	if compLen >= rawLen-rawLen/20 {
		return true
	}
	storeLen := 4 + 5 + rawLen
	if compLen >= storeLen {
		return true
	}
	return false
}

func lzwCompress(raw []byte) []byte {
	comp := lzwCompressCore(raw)
	if lzwShouldStore(len(raw), len(comp)) {
		return lzwStoreMode(raw)
	}
	return comp
}

func lzwCompressCore(raw []byte) []byte {
	header := make([]byte, 0, 12)
	header = append(header, 'L', 'Z', 'W', '1')
	var tmp [binary.MaxVarintLen64]byte
	header = append(header, tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))]...)

	bw := newLZWBitWriter(len(raw))
	var h lzwFastHash
	ac := newLZWAdaptiveClear()
	nextCode := int32(lzwClearCode + 1)
	nbits := uint(9)

	var w int32 = -1
	epochIn := 0
	epochOut := 0

	i := 0
	for i < len(raw) {
		b := raw[i]
		if i+1 < len(raw) && raw[i+1] == b {
			if b2, runLen := lzwDetectRun(raw, i); runLen > 0 {
				if w >= 0 {
					bw.write(uint32(w), nbits)
					w = -1
				}
				lzwEncodeRun(bw, b2, runLen, nbits)
				i += runLen
				continue
			}
		}
		var key uint32
		if w < 0 {
			key = 0xFFFFFF00 | uint32(b)
		} else {
			key = uint32(w)<<8 | uint32(b)
		}

		code, ok := h.lookup(key)
		if ok {
			w = code
			i++
			continue
		}

		if w < 0 {
			bw.write(uint32(b), nbits)
		} else {
			bw.write(uint32(w), nbits)
		}

		if nextCode < lzwMaxCode {
			h.insert(key, nextCode)
			nextCode++
			if nextCode > int32(1<<nbits) && nbits < 12 {
				nbits++
			}
		}
		w = int32(b)
		i++

		if nextCode-lzwClearCode-1 >= lzwEpochLen {
			inLen := i - epochIn
			outLen := bw.outPos - epochOut
			if bw.bits > 0 {
				outLen++
			}
			if inLen > 64 && outLen > 0 {
				ratio := float64(inLen) / float64(outLen)
				if ac.shouldReset(ratio) {
					bw.write(lzwClearCode, nbits)
					h.reset()
					nextCode = lzwClearCode + 1
					nbits = 9
					epochIn = i
					epochOut = bw.outPos
					ac.reset()
				} else {
					epochIn = i
					epochOut = bw.outPos
				}
			}
		}
	}
	if w >= 0 {
		bw.write(uint32(w), nbits)
	}
	body := bw.flush()
	return append(header, body...)
}

func lzwCompressParallel(raw []byte) []byte {
	numWorkers := lzwParallelChunks
	const minChunk = 512 * 1024
	if numWorkers*minChunk > len(raw) {
		numWorkers = len(raw) / minChunk
		if numWorkers < 1 {
			numWorkers = 1
		}
	}
	chunkSize := (len(raw) + numWorkers - 1) / numWorkers

	results := make([][]byte, numWorkers)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		start := i * chunkSize
		end := (i + 1) * chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		if start >= end {
			results[i] = nil
			continue
		}
		wg.Add(1)
		go func(idx int, data []byte) {
			defer wg.Done()
			results[idx] = lzwCompressCore(data)
		}(i, raw[start:end])
	}
	wg.Wait()

	nonEmpty := 0
	for _, r := range results {
		if r != nil {
			nonEmpty++
		}
	}

	totalSize := 8
	for _, r := range results {
		if r != nil {
			totalSize += len(r) + 8
		}
	}
	out := make([]byte, 0, totalSize)
	out = append(out, 'L', 'Z', 'W', 'P')
	var tmp [binary.MaxVarintLen64]byte
	out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))]...)
	out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(nonEmpty))]...)
	for _, r := range results {
		if r == nil {
			continue
		}
		out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(len(r)))]...)
		out = append(out, r...)
	}

	if lzwShouldStore(len(raw), len(out)) {
		return lzwStoreMode(raw)
	}
	return out
}

func lzwDecompress(comp []byte) ([]byte, error) {
	if bytes.HasPrefix(comp, []byte("LZWP")) {
		return lzwDecompressParallel(comp)
	}
	if bytes.HasPrefix(comp, []byte("LZWS")) {
		rest := comp[4:]
		n64, k := binary.Uvarint(rest)
		if k <= 0 || n64 > 1<<31 {
			return nil, errors.New("LZWS header corrupted")
		}
		n := int(n64)
		if len(rest) < k+n {
			return nil, errors.New("LZWS data truncated")
		}
		return rest[k : k+n], nil
	}
	if !bytes.HasPrefix(comp, []byte("LZW1")) {
		return nil, errors.New("not LZW1/LZWS/LZWP data")
	}
	rest := comp[4:]
	n64, k := binary.Uvarint(rest)
	if k <= 0 || n64 > 1<<31 {
		return nil, errors.New("LZW1 header corrupted")
	}
	n := int(n64)
	if n == 0 {
		return []byte{}, nil
	}
	br := &lzwBitReader{data: rest[k:]}

	var dictPrefix [lzwMaxCode]int32
	var dictByte [lzwMaxCode]byte
	var dictLen [lzwMaxCode]int32
	for i := 0; i < 256; i++ {
		dictPrefix[i] = -1
		dictByte[i] = byte(i)
		dictLen[i] = 1
	}
	for i := 256; i < lzwMaxCode; i++ {
		dictPrefix[i] = -1
	}

	nextCode := int32(lzwClearCode + 1)
	nbits := uint(9)

	out := make([]byte, 0, n+64)
	var stack [lzwMaxCode + 16]byte

	first, ok := br.read(nbits)
	if !ok {
		return nil, errors.New("LZW1 too short")
	}
	if first >= lzwRunBase && first < lzwRunBase+256 {
		runLen, err := lzwDecodeRun(br, nbits)
		if err != nil {
			return nil, err
		}
		if runLen > n {
			runLen = n
		}
		b := byte(first - lzwRunBase)
		for k := 0; k < runLen; k++ {
			out = append(out, b)
		}
		if len(out) >= n {
			return out[:n], nil
		}
		c2, ok := br.read(nbits)
		if !ok {
			return out, nil
		}
		if c2 >= lzwRunBase && c2 < lzwRunBase+256 {
			first = c2
			runLen2, err := lzwDecodeRun(br, nbits)
			if err != nil {
				return nil, err
			}
			if int32(len(out))+int32(runLen2) > int32(n) {
				runLen2 = n - len(out)
			}
			b2 := byte(c2 - lzwRunBase)
			for k := 0; k < runLen2; k++ {
				out = append(out, b2)
			}
			if len(out) >= n {
				return out[:n], nil
			}
			first, ok = br.read(nbits)
			if !ok {
				return out, nil
			}
		} else {
			first = c2
		}
	}
	if first == lzwClearCode {
		c2, ok := br.read(nbits)
		if !ok {
			return []byte{}, nil
		}
		if c2 >= 256 {
			return nil, errors.New("after clear, must be literal")
		}
		out = append(out, byte(c2))
		if int32(len(out)) >= int32(n) {
			return out[:n], nil
		}
		first = c2
	} else if first >= 256 && (first < lzwRunBase || first >= lzwRunBase+256) {
		return nil, fmt.Errorf("first code must be < 256, got %d", first)
	} else if first < 256 {
		out = append(out, byte(first))
	}
	var w int32 = int32(first)

	for int32(len(out)) < int32(n) {
		c, ok := br.read(nbits)
		if !ok {
			break
		}
		code := int32(c)

		if code >= lzwRunBase && code < lzwRunBase+256 {
			runLen, err := lzwDecodeRun(br, nbits)
			if err != nil {
				return nil, err
			}
			b := byte(code - lzwRunBase)
			if int32(len(out))+int32(runLen) > int32(n) {
				runLen = n - len(out)
			}
			for k := 0; k < runLen; k++ {
				out = append(out, b)
			}
			w = -1
			continue
		}

		if code == lzwClearCode {
			for i := 256; i < lzwMaxCode; i++ {
				dictPrefix[i] = -1
			}
			nextCode = lzwClearCode + 1
			nbits = 9
			c2, ok := br.read(nbits)
			if !ok {
				break
			}
			if c2 >= 256 {
				return nil, errors.New("after clear, must be literal")
			}
			out = append(out, byte(c2))
			w = int32(c2)
			continue
		}

		var entryLen int32
		var entryCode int32
		kwkwk := false
		if code < nextCode {
			entryLen = dictLen[code]
			entryCode = code
		} else if code == nextCode && w >= 0 {
			entryLen = dictLen[w] + 1
			entryCode = w
			kwkwk = true
		} else {
			return nil, fmt.Errorf("invalid LZW code: %d (next=%d)", code, nextCode)
		}

		if int32(len(out))+entryLen > int32(n)+int32(lzwMaxCode) {
			return nil, errors.New("LZW1: output too large")
		}

		sp := int(entryLen)
		ec := entryCode
		for ec >= 0 && sp > 0 {
			sp--
			stack[sp] = dictByte[ec]
			ec = dictPrefix[ec]
		}
		firstByte := stack[sp]
		out = append(out, stack[sp:entryLen]...)

		if nextCode < lzwMaxCode && w >= 0 {
			dictPrefix[nextCode] = w
			dictByte[nextCode] = firstByte
			dictLen[nextCode] = dictLen[w] + 1
			nextCode++
			if nextCode >= int32(1<<nbits) && nbits < 12 {
				nbits++
			}
		}
		w = code
		if kwkwk {
			w = nextCode - 1
		}
	}

	if int32(len(out)) > int32(n) {
		out = out[:n]
	}
	if int32(len(out)) < int32(n) {
		return nil, fmt.Errorf("LZW1: data truncated (%d/%d)", len(out), n)
	}
	return out, nil
}

func lzwDecompressParallel(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("LZWP")) {
		return nil, errors.New("not LZWP data")
	}
	rest := comp[4:]
	total64, k1 := binary.Uvarint(rest)
	if k1 <= 0 {
		return nil, errors.New("LZWP: header total corrupted")
	}
	total := int(total64)
	rest = rest[k1:]

	num64, k2 := binary.Uvarint(rest)
	if k2 <= 0 {
		return nil, errors.New("LZWP: header chunks corrupted")
	}
	num := int(num64)
	rest = rest[k2:]

	out := make([]byte, 0, total)
	for i := 0; i < num; i++ {
		chunkLen64, k := binary.Uvarint(rest)
		if k <= 0 || int(chunkLen64) > len(rest) {
			return nil, fmt.Errorf("LZWP: chunk %d corrupted", i)
		}
		rest = rest[k:]
		chunkData := rest[:chunkLen64]
		rest = rest[chunkLen64:]
		dec, err := lzwDecompress(chunkData)
		if err != nil {
			return nil, fmt.Errorf("LZWP: chunk %d: %w", i, err)
		}
		out = append(out, dec...)
	}
	if len(out) < total {
		return nil, fmt.Errorf("LZWP: data truncated (%d/%d)", len(out), total)
	}
	return out[:total], nil
}

// ======================================================================
// LZH v1: LZW + Huffman
// ======================================================================

func lzwEncodeToCodes(raw []byte) []int32 {
	if len(raw) == 0 {
		return nil
	}
	codes := make([]int32, 0, len(raw))
	var h lzwFastHash
	nextCode := int32(lzwClearCode + 1)

	w := int32(raw[0])
	for i := 1; i < len(raw); i++ {
		b := raw[i]
		key := uint32(w)<<8 | uint32(b)
		if code, ok := h.lookup(key); ok {
			w = code
			continue
		}
		codes = append(codes, w)
		if nextCode < lzwMaxCode {
			h.insert(key, nextCode)
			nextCode++
		}
		w = int32(b)
	}
	codes = append(codes, w)
	return codes
}

func lzwDecodeCodes(codes []int32) []byte {
	if len(codes) == 0 {
		return nil
	}
	var dictPrefix [lzwMaxCode]int32
	var dictByte [lzwMaxCode]byte
	var dictLen [lzwMaxCode]int32
	for i := 0; i < 256; i++ {
		dictPrefix[i] = -1
		dictByte[i] = byte(i)
		dictLen[i] = 1
	}
	for i := 256; i < lzwMaxCode; i++ {
		dictPrefix[i] = -1
	}

	nextCode := int32(lzwClearCode + 1)
	var out []byte
	var stack [lzwMaxCode + 16]byte

	first := codes[0]
	if first >= 256 {
		return nil
	}
	out = append(out, byte(first))
	w := first

	for i := 1; i < len(codes); i++ {
		code := codes[i]

		var entryLen int32
		var entryCode int32
		kwkwk := false
		if code < nextCode {
			entryLen = dictLen[code]
			entryCode = code
		} else if code == nextCode && w >= 0 {
			entryLen = dictLen[w] + 1
			entryCode = w
			kwkwk = true
		} else {
			return nil
		}

		sp := int(entryLen)
		if kwkwk {
			sp = int(dictLen[w])
		}
		ec := entryCode
		for ec >= 0 && sp > 0 {
			sp--
			stack[sp] = dictByte[ec]
			ec = dictPrefix[ec]
		}
		firstByte := stack[0]

		if kwkwk {
			wLen := int(dictLen[w])
			out = append(out, stack[0:wLen]...)
			out = append(out, stack[0])
		} else {
			out = append(out, stack[0:entryLen]...)
		}

		if nextCode < lzwMaxCode {
			dictPrefix[nextCode] = w
			dictByte[nextCode] = firstByte
			dictLen[nextCode] = dictLen[w] + 1
			nextCode++
		}
		w = code
		if kwkwk {
			w = nextCode - 1
		}
	}
	return out
}

func lzhCompress(raw []byte) []byte {
	codes := lzwEncodeToCodes(raw)
	if len(codes) == 0 {
		return nil
	}

	freq := make(map[int32]int, 4096)
	for _, c := range codes {
		freq[c]++
	}

	type hn struct {
		freq   int
		code   int32
		left   int32
		right  int32
		isLeaf bool
	}
	var tree []hn
	var heap []int32

	for code, f := range freq {
		tree = append(tree, hn{freq: f, code: code, left: -1, right: -1, isLeaf: true})
		heap = append(heap, int32(len(tree)-1))
	}
	sort.Slice(heap, func(i, j int) bool {
		return tree[heap[i]].freq < tree[heap[j]].freq
	})

	for len(heap) > 1 {
		a, b := heap[0], heap[1]
		heap = heap[2:]
		tree = append(tree, hn{
			freq:  tree[a].freq + tree[b].freq,
			left:  a,
			right: b,
			code:  -1,
		})
		ni := int32(len(tree) - 1)
		lo, hi := 0, len(heap)
		for lo < hi {
			mid := (lo + hi) / 2
			if tree[heap[mid]].freq < tree[ni].freq {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		heap = append(heap, 0)
		copy(heap[lo+1:], heap[lo:])
		heap[lo] = ni
	}

	root := heap[0]

	type hc struct {
		code int32
		len  uint8
	}
	huffCodes := make(map[int32]hc, len(freq))
	var walk func(idx int32, c int32, l uint8)
	walk = func(idx int32, c int32, l uint8) {
		n := &tree[idx]
		if n.isLeaf {
			if l == 0 {
				l = 1
			}
			huffCodes[n.code] = hc{c, l}
			return
		}
		walk(n.left, c<<1, l+1)
		walk(n.right, (c<<1)|1, l+1)
	}
	walk(root, 0, 0)

	var treeBuf bytes.Buffer
	var tmp [binary.MaxVarintLen64]byte
	treeBuf.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(tree)))])
	for _, n := range tree {
		if n.isLeaf {
			treeBuf.WriteByte(1)
			treeBuf.Write(tmp[:binary.PutUvarint(tmp[:], uint64(uint32(n.code)))])
		} else {
			treeBuf.WriteByte(0)
			var b4 [4]byte
			binary.LittleEndian.PutUint32(b4[:], uint32(n.left))
			treeBuf.Write(b4[:])
			binary.LittleEndian.PutUint32(b4[:], uint32(n.right))
			treeBuf.Write(b4[:])
		}
	}

	var out bytes.Buffer
	out.WriteString("LZH1")
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))])
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(codes)))])
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(treeBuf.Len()))])
	out.Write(treeBuf.Bytes())

	var bitbuf uint64
	var bitcnt uint
	for _, c := range codes {
		h := huffCodes[c]
		bitbuf = (bitbuf << h.len) | uint64(h.code)
		bitcnt += uint(h.len)
		for bitcnt >= 8 {
			bitcnt -= 8
			out.WriteByte(byte(bitbuf >> bitcnt))
			bitbuf &= (1 << bitcnt) - 1
		}
	}
	if bitcnt > 0 {
		out.WriteByte(byte(bitbuf << (8 - bitcnt)))
	}
	return out.Bytes()
}

func lzhDecompress(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("LZH1")) {
		return nil, errors.New("not LZH1 data")
	}
	rest := comp[4:]

	rawLen64, k := binary.Uvarint(rest)
	if k <= 0 || rawLen64 > 1<<31 {
		return nil, errors.New("LZH1: bad rawLen")
	}
	rawLen := int(rawLen64)
	rest = rest[k:]

	numCodes64, k := binary.Uvarint(rest)
	if k <= 0 || numCodes64 > 1<<30 {
		return nil, errors.New("LZH1: bad numCodes")
	}
	numCodes := int(numCodes64)
	rest = rest[k:]

	treeLen64, k := binary.Uvarint(rest)
	if k <= 0 || treeLen64 > uint64(len(rest)) {
		return nil, errors.New("LZH1: bad treeLen")
	}
	treeLen := int(treeLen64)
	rest = rest[k:]

	if treeLen > len(rest) {
		return nil, errors.New("LZH1: tree too long")
	}
	treeBytes := rest[:treeLen]
	bitStream := rest[treeLen:]

	tp := 0
	numNodes64, tk := binary.Uvarint(treeBytes[tp:])
	if tk <= 0 || numNodes64 > 8192 {
		return nil, errors.New("LZH1: bad node count")
	}
	tp += tk
	numNodes := int(numNodes64)

	type node struct {
		code   int32
		left   int32
		right  int32
		isLeaf bool
	}
	tree := make([]node, numNodes)
	for i := 0; i < numNodes; i++ {
		if tp >= len(treeBytes) {
			return nil, errors.New("LZH1: tree truncated")
		}
		isLeaf := treeBytes[tp] == 1
		tp++
		if isLeaf {
			code, k := binary.Uvarint(treeBytes[tp:])
			if k <= 0 {
				return nil, errors.New("LZH1: bad leaf")
			}
			tp += k
			tree[i] = node{code: int32(code), isLeaf: true, left: -1, right: -1}
		} else {
			if tp+8 > len(treeBytes) {
				return nil, errors.New("LZH1: internal truncated")
			}
			left := int32(binary.LittleEndian.Uint32(treeBytes[tp:]))
			right := int32(binary.LittleEndian.Uint32(treeBytes[tp+4:]))
			tp += 8
			tree[i] = node{left: left, right: right}
		}
	}

	root := int32(numNodes - 1)

	var bitpos int
	readBit := func() (int, bool) {
		byteIdx := bitpos / 8
		if byteIdx >= len(bitStream) {
			return 0, false
		}
		bitIdx := 7 - uint(bitpos%8)
		bit := int(bitStream[byteIdx]>>bitIdx) & 1
		bitpos++
		return bit, true
	}

	codes := make([]int32, 0, numCodes)
	for i := 0; i < numCodes; i++ {
		cur := root
		for !tree[cur].isLeaf {
			b, ok := readBit()
			if !ok {
				return nil, errors.New("LZH1: bits truncated")
			}
			if b == 0 {
				cur = tree[cur].left
			} else {
				cur = tree[cur].right
			}
		}
		codes = append(codes, tree[cur].code)
	}

	raw := lzwDecodeCodes(codes)
	if raw == nil {
		return nil, errors.New("LZH1: LZW decode failed")
	}
	if len(raw) < rawLen {
		return nil, fmt.Errorf("LZH1: got %d want %d", len(raw), rawLen)
	}
	return raw[:rawLen], nil
}

// ======================================================================
// LZW v2: preset dictionary
// ======================================================================

var dnaPreset = []string{
	"AAAAAAAAAAAA", "TTTTTTTTTTTT", "GGGGGGGGGGGG", "CCCCCCCCCCCC",
	"ATATATATATAT", "GCGCGCGCGCGC", "ATGCATGCATGC", "NNNNNNNNNNNN",
}

var textPreset = []string{
	"the ", "and ", "that ", "have ", "with ", "this ", "from ",
	"they ", "will ", "would ", "there ", "their ", "what ", "when ",
	"about ", "which ", "make ", "like ", "time ", "just ", "know ",
}

var javaPreset = []string{
	"public ", "private ", "protected ", "static ", "final ",
	"class ", "interface ", "extends ", "implements ",
	"return ", "import ", "package ", "void ", "int ",
	"String ", "boolean ", "@Override", "@Autowired",
	"System.out.println",
}

func detectPreset(data []byte) ([]string, byte) {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	if n == 0 {
		return nil, 0
	}
	dnaCount, atgc := 0, 0
	for _, b := range data[:n] {
		switch b {
		case 'A', 'T', 'G', 'C', 'N', 'a', 't', 'g', 'c', 'n':
			dnaCount++
			atgc++
		case '>', '\n', ' ', '\r', '\t', ';':
			dnaCount++
		}
	}
	if dnaCount > n*9/10 && atgc > n*7/10 {
		return dnaPreset, 1
	}
	hits := 0
	for _, kw := range javaPreset[:10] {
		if bytes.Contains(data[:n], []byte(kw)) {
			hits++
		}
	}
	if hits >= 3 {
		return javaPreset, 3
	}
	return textPreset, 2
}

func applyPresetEncoder(h *lzwFastHash, preset []string) int32 {
	nextCode := int32(lzwClearCode + 1)
	for _, s := range preset {
		if len(s) < 2 {
			continue
		}
		prevCode := int32(s[0])
		for i := 1; i < len(s); i++ {
			key := uint32(prevCode)<<8 | uint32(s[i])
			if code, ok := h.lookup(key); ok {
				prevCode = code
				continue
			}
			if nextCode >= lzwMaxCode {
				return nextCode
			}
			h.insert(key, nextCode)
			prevCode = nextCode
			nextCode++
		}
	}
	return nextCode
}

func applyPresetDecoder(dictPrefix *[lzwMaxCode]int32, dictByte *[lzwMaxCode]byte, dictLen *[lzwMaxCode]int32, preset []string) int32 {
	var h lzwFastHash
	nextCode := int32(lzwClearCode + 1)
	for _, s := range preset {
		if len(s) < 2 {
			continue
		}
		prevCode := int32(s[0])
		for i := 1; i < len(s); i++ {
			key := uint32(prevCode)<<8 | uint32(s[i])
			if code, ok := h.lookup(key); ok {
				prevCode = code
				continue
			}
			if nextCode >= lzwMaxCode {
				return nextCode
			}
			h.insert(key, nextCode)
			dictPrefix[nextCode] = prevCode
			dictByte[nextCode] = s[i]
			dictLen[nextCode] = dictLen[prevCode] + 1
			prevCode = nextCode
			nextCode++
		}
	}
	return nextCode
}

func lzwCompressWithPreset(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	preset, presetID := detectPreset(raw)
	if preset == nil {
		return nil
	}

	header := make([]byte, 0, 20)
	header = append(header, 'L', 'Z', 'W', '2', presetID)
	var tmp [binary.MaxVarintLen64]byte
	header = append(header, tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))]...)

	bw := newLZWBitWriter(len(raw))
	var h lzwFastHash
	nextCode := applyPresetEncoder(&h, preset)

	nbits := uint(9)
	for (int32(1)<<nbits) < nextCode && nbits < 12 {
		nbits++
	}

	w := int32(raw[0])
	for i := 1; i < len(raw); i++ {
		b := raw[i]
		key := uint32(w)<<8 | uint32(b)
		if code, ok := h.lookup(key); ok {
			w = code
			continue
		}
		bw.write(uint32(w), nbits)
		if nextCode < lzwMaxCode {
			h.insert(key, nextCode)
			nextCode++
			if nextCode > (1<<nbits) && nbits < 12 {
				nbits++
			}
		}
		w = int32(b)
	}
	bw.write(uint32(w), nbits)
	body := bw.flush()

	if len(header)+len(body) > len(raw)+len(raw)/50 {
		return nil
	}
	return append(header, body...)
}

func lzwDecompressWithPreset(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("LZW2")) {
		return nil, errors.New("not LZW2 data")
	}
	rest := comp[4:]
	if len(rest) < 1 {
		return nil, errors.New("LZW2: missing presetID")
	}
	presetID := rest[0]
	rest = rest[1:]

	var preset []string
	switch presetID {
	case 1:
		preset = dnaPreset
	case 2:
		preset = textPreset
	case 3:
		preset = javaPreset
	default:
		return nil, fmt.Errorf("LZW2: unknown preset %d", presetID)
	}

	rawLen64, k := binary.Uvarint(rest)
	if k <= 0 || rawLen64 > 1<<31 {
		return nil, errors.New("LZW2: bad rawLen")
	}
	n := int(rawLen64)
	rest = rest[k:]

	var dictPrefix [lzwMaxCode]int32
	var dictByte [lzwMaxCode]byte
	var dictLen [lzwMaxCode]int32
	for i := 0; i < 256; i++ {
		dictPrefix[i] = -1
		dictByte[i] = byte(i)
		dictLen[i] = 1
	}
	for i := 256; i < lzwMaxCode; i++ {
		dictPrefix[i] = -1
	}
	nextCode := applyPresetDecoder(&dictPrefix, &dictByte, &dictLen, preset)

	br := &lzwBitReader{data: rest}
	nbits := uint(9)
	for (int32(1)<<nbits) < nextCode && nbits < 12 {
		nbits++
	}

	out := make([]byte, 0, n+64)
	var stack [lzwMaxCode + 16]byte

	first, ok := br.read(nbits)
	if !ok {
		return nil, errors.New("LZW2: too short")
	}
	if int32(first) >= nextCode {
		return nil, fmt.Errorf("LZW2: first code %d >= nextCode %d", first, nextCode)
	}
	{
		entryLen := dictLen[first]
		ec := int32(first)
		sp := int(entryLen)
		for ec >= 0 && sp > 0 {
			sp--
			stack[sp] = dictByte[ec]
			ec = dictPrefix[ec]
		}
		out = append(out, stack[0:entryLen]...)
	}
	w := int32(first)

	for int32(len(out)) < int32(n) {
		c, ok := br.read(nbits)
		if !ok {
			break
		}
		code := int32(c)

		var entryLen int32
		var entryCode int32
		kwkwk := false
		if code < nextCode {
			entryLen = dictLen[code]
			entryCode = code
		} else if code == nextCode && w >= 0 {
			entryLen = dictLen[w] + 1
			entryCode = w
			kwkwk = true
		} else {
			return nil, fmt.Errorf("LZW2: invalid code %d (next=%d)", code, nextCode)
		}

		sp := int(entryLen)
		if kwkwk {
			sp = int(dictLen[w])
		}
		ec := entryCode
		for ec >= 0 && sp > 0 {
			sp--
			stack[sp] = dictByte[ec]
			ec = dictPrefix[ec]
		}
		firstByte := stack[0]

		if kwkwk {
			wLen := int(dictLen[w])
			out = append(out, stack[0:wLen]...)
			out = append(out, stack[0])
		} else {
			out = append(out, stack[0:entryLen]...)
		}

		if nextCode < lzwMaxCode && w >= 0 {
			dictPrefix[nextCode] = w
			dictByte[nextCode] = firstByte
			dictLen[nextCode] = dictLen[w] + 1
			nextCode++
			if nextCode >= int32(1<<nbits) && nbits < 12 {
				nbits++
			}
		}
		w = code
		if kwkwk {
			w = nextCode - 1
		}
	}

	if int32(len(out)) > int32(n) {
		out = out[:n]
	}
	if int32(len(out)) < int32(n) {
		return nil, fmt.Errorf("LZW2: got %d want %d", len(out), n)
	}
	return out, nil
}

// ======================================================================
// DNA mode: pack 2-bit + RLE escapes for non-ACGT bases
// ======================================================================
// Format: "DNA2" + uvarint(numHeaders) + [uvarint(hlen) + header]*
//         + uvarint(seqLen) + packedBits + uvarint(numEscapes)
//         + [uvarint(delta) + uvarint(runLen) + byte]*
// Bases: A=0, T=1, G=2, C=3. Non-ACGT → C placeholder + escape run.

func looksDNA(data []byte) bool {
	n := len(data)
	if n < 1024 {
		return false
	}
	sample := data
	if n > 65536 {
		sample = data[:65536]
	}
	atgc := 0
	total := 0
	inHeader := false
	for i, b := range sample {
		if b == '>' && (i == 0 || sample[i-1] == '\n') {
			inHeader = true
			continue
		}
		if b == '\n' || b == '\r' {
			inHeader = false
			continue
		}
		if inHeader {
			continue
		}
		if b == ' ' || b == '\t' {
			continue
		}
		total++
		switch b {
		case 'A', 'T', 'G', 'C', 'a', 't', 'g', 'c',
			'N', 'n', 'U', 'u', 'R', 'Y', 'S', 'W',
			'K', 'M', 'B', 'D', 'H', 'V',
			'r', 'y', 's', 'w', 'k', 'm', 'b', 'd', 'h', 'v':
			atgc++
		default:
			return false
		}
	}
	return total > 0 && atgc*100/total > 90
}

// packDNA2Bit: FASTA/plain DNA → 2-bit packed with RLE escape runs.
func packDNA2Bit(data []byte) []byte {
	var out bytes.Buffer
	out.WriteString("DNA2")

	// Parse FASTA: extract headers and sequence
	var headers []string
	var seq []byte
	var curHeader []byte
	inHeader := false

	for i := 0; i < len(data); i++ {
		b := data[i]
		if b == '>' && (i == 0 || data[i-1] == '\n') {
			if inHeader && len(curHeader) > 0 {
				headers = append(headers, string(curHeader))
			}
			curHeader = []byte{}
			inHeader = true
			continue
		}
		if b == '\n' || b == '\r' {
			if inHeader {
				if len(curHeader) > 0 {
					headers = append(headers, string(curHeader))
				}
				curHeader = nil
				inHeader = false
			}
			continue
		}
		if inHeader {
			curHeader = append(curHeader, b)
			continue
		}
		if b == ' ' || b == '\t' {
			continue
		}
		seq = append(seq, b)
	}
	if inHeader && len(curHeader) > 0 {
		headers = append(headers, string(curHeader))
	}

	var tmp [binary.MaxVarintLen64]byte

	// Headers
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(headers)))])
	for _, h := range headers {
		out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(h)))])
		out.WriteString(h)
	}

	// Sequence length
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(seq)))])

	// Pack sequence + RLE escapes
	type escape struct {
		pos  uint64
		len  uint64
		byte byte
	}
	var escapes []escape
	var curRun *escape

	packed := make([]byte, 0, (len(seq)+3)/4)
	var curByte byte
	var curBits uint

	for i, b := range seq {
		var code byte
		isEsc := false
		var escByte byte

		switch b {
		case 'A', 'a':
			code = 0
		case 'T', 't', 'U', 'u':
			code = 1
		case 'G', 'g':
			code = 2
		case 'C', 'c':
			code = 3
		default:
			code = 3 // placeholder, escaped below
			isEsc = true
			escByte = b
		}

		if isEsc {
			if curRun != nil && curRun.byte == escByte && curRun.pos+curRun.len == uint64(i) {
				curRun.len++
			} else {
				if curRun != nil {
					escapes = append(escapes, *curRun)
				}
				curRun = &escape{pos: uint64(i), len: 1, byte: escByte}
			}
		} else {
			if curRun != nil {
				escapes = append(escapes, *curRun)
				curRun = nil
			}
		}

		curByte = (curByte << 2) | code
		curBits += 2
		if curBits == 8 {
			packed = append(packed, curByte)
			curByte = 0
			curBits = 0
		}
	}
	if curBits > 0 {
		for curBits < 8 {
			curByte <<= 2
			curBits += 2
		}
		packed = append(packed, curByte)
	}
	if curRun != nil {
		escapes = append(escapes, *curRun)
		curRun = nil
	}

	out.Write(packed)

	// Escape runs (delta-encoded)
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(escapes)))])
	var prevEnd uint64
	for _, e := range escapes {
		out.Write(tmp[:binary.PutUvarint(tmp[:], e.pos-prevEnd)])
		out.Write(tmp[:binary.PutUvarint(tmp[:], e.len)])
		out.WriteByte(e.byte)
		prevEnd = e.pos + e.len
	}

	return out.Bytes()
}

// unpackDNA2Bit: reverse of packDNA2Bit, reconstruct FASTA text.
func unpackDNA2Bit(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("DNA2")) {
		return nil, errors.New("not DNA2 data")
	}
	rest := comp[4:]

	numHeaders, k := binary.Uvarint(rest)
	if k <= 0 {
		return nil, errors.New("DNA2: bad numHeaders")
	}
	rest = rest[k:]

	var headers []string
	for i := uint64(0); i < numHeaders; i++ {
		hlen, k := binary.Uvarint(rest)
		if k <= 0 || uint64(len(rest)) < uint64(k)+hlen {
			return nil, errors.New("DNA2: bad header")
		}
		rest = rest[k:]
		headers = append(headers, string(rest[:hlen]))
		rest = rest[hlen:]
	}

	seqLen, k := binary.Uvarint(rest)
	if k <= 0 {
		return nil, errors.New("DNA2: bad seqLen")
	}
	rest = rest[k:]

	packedLen := (int(seqLen) + 3) / 4
	if len(rest) < packedLen {
		return nil, errors.New("DNA2: packed truncated")
	}
	packed := rest[:packedLen]
	rest = rest[packedLen:]

	seq := make([]byte, seqLen)
	const bases = "ATGC"
	for i := 0; i < int(seqLen); i++ {
		byteIdx := i / 4
		shift := uint((3 - (i % 4)) * 2)
		code := (packed[byteIdx] >> shift) & 0x3
		seq[i] = bases[code]
	}

	numEscapes, k := binary.Uvarint(rest)
	if k <= 0 {
		return nil, errors.New("DNA2: bad numEscapes")
	}
	rest = rest[k:]

	var pos uint64
	for i := uint64(0); i < numEscapes; i++ {
		delta, k := binary.Uvarint(rest)
		if k <= 0 {
			return nil, errors.New("DNA2: bad delta")
		}
		rest = rest[k:]

		runLen, k := binary.Uvarint(rest)
		if k <= 0 {
			return nil, errors.New("DNA2: bad runLen")
		}
		rest = rest[k:]

		if len(rest) < 1 {
			return nil, errors.New("DNA2: escape byte truncated")
		}
		escByte := rest[0]
		rest = rest[1:]

		start := pos + delta
		if start+runLen > seqLen {
			return nil, errors.New("DNA2: escape overflow")
		}
		for j := uint64(0); j < runLen; j++ {
			seq[start+j] = escByte
		}
		pos = start + runLen
	}

	// Reconstruct FASTA
	var out bytes.Buffer
	if len(headers) > 0 {
		for _, h := range headers {
			out.WriteByte('>')
			out.WriteString(h)
			out.WriteByte('\n')
			for i := 0; i < len(seq); i += 60 {
				end := i + 60
				if end > len(seq) {
					end = len(seq)
				}
				out.Write(seq[i:end])
				out.WriteByte('\n')
			}
		}
	} else {
		out.Write(seq)
	}
	return out.Bytes(), nil
}

// ======================================================================
// CMX1 v3 - context mixing (lpaq-style, reduced prototype made stronger)
//
// Compared with v2:
//   - nibble-bucketed hash tables with check word + 2-way replacement
//     (fewer collisions, unseen contexts give no input instead of noise)
//   - two inputs per context: stretch(p) and (p-0.5)
//   - extra contexts: word/bigram and sparse (skip) context
//   - match model: verified length, learned confidence, stops predicting
//     for the rest of a byte after the first wrong bit
//   - 3 mixers (selected by c0, match state, previous byte) + 2nd layer
//   - 3 APM/SSE stages working in the stretch domain
//   - adaptive-rate counters via reciprocal table
// NOTE: format changed, old CMX1 v2 streams are not decodable.
// ======================================================================

var stretchTab [4096]int16

var squashTab [4096]int16

// squash maps the stretch domain [-2047,2047] to a 12-bit probability (table lookup).
func squash(d int) int {
	if d > 2047 {
		d = 2047
	} else if d < -2047 {
		d = -2047
	}
	return int(squashTab[d+2048])
}

func squashSlow(d int) int {
	if d > 2047 {
		return 4095
	}
	if d < -2047 {
		return 0
	}
	t := [33]int{1, 2, 3, 6, 10, 16, 27, 45, 73, 120, 194, 310, 488, 747, 1101,
		1546, 2047, 2549, 2994, 3348, 3607, 3785, 3901, 3975, 4022,
		4050, 4068, 4079, 4085, 4089, 4092, 4093, 4094}
	w := d & 127
	d = (d >> 7) + 16
	return (t[d]*(128-w) + t[d+1]*w + 64) >> 7
}

var cmDt [1024]int32

func init() {
	for x := -2047; x <= 2047; x++ {
		squashTab[x+2048] = int16(squashSlow(x))
	}
	pi := 0
	for x := -2047; x <= 2047; x++ {
		v := squash(x)
		for i := pi; i <= v; i++ {
			stretchTab[i] = int16(x)
		}
		pi = v + 1
	}
	for i := pi; i < 4096; i++ {
		stretchTab[i] = 2047
	}
	for n := range cmDt {
		cmDt[n] = int32(131072 / (2*n + 3)) // 65536/(n+1.5)
	}
}

const (
	cmHashed = 7             // o2,o3,o4,o5,o6,word,sparse
	cmCtx    = 2 + cmHashed  // + direct o0, o1
	cmNIn    = cmCtx*2 + 3   // 2 per ctx + 2 match + bias
	cmSetsA  = 256           // mixer A selected by c0
	cmSetsB  = 128           // mixer B selected by match length / bit position
	cmSetsC  = 256           // mixer C selected by previous byte
)

// cmH mixes a 64-bit key into a well-distributed 32-bit hash.
func cmH(x uint64) uint32 {
	x *= 0x9E3779B97F4A7C15
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return uint32(x)
}

// cmCtr: high 16 bits = P(1), low 16 bits = hit count (n==0 means unseen).
type cmCtr uint32

const cmCtrInit = cmCtr(uint32(32768) << 16)

func (c *cmCtr) upd(y, limit int) {
	n := int(*c & 0xFFFF)
	p := int(*c >> 16)
	p += int((int64(y*65535-p) * int64(cmDt[n])) >> 16)
	if p < 16 {
		p = 16
	} else if p > 65519 {
		p = 65519
	}
	if n < limit {
		n++
	}
	*c = cmCtr(uint32(p)<<16 | uint32(n))
}

// cmIn turns a counter into two mixer inputs; unseen contexts give 0,0.
func cmIn(c cmCtr) (int, int) {
	if c&0xFFFF == 0 {
		return 0, 0
	}
	p := int(c>>16) >> 4
	return int(stretchTab[p]), p - 2048
}

// cmAPM refines a probability given a context, interpolating in the
// stretch domain (33 bins) and updating the nearer bin.
type cmAPM struct {
	t   []uint16
	idx int
}

func newAPM(n int) *cmAPM {
	a := &cmAPM{t: make([]uint16, n*33)}
	var row [33]uint16
	for j := 0; j < 33; j++ {
		row[j] = uint16(squash((j-16)*128) * 16)
	}
	for i := 0; i < n; i++ {
		copy(a.t[i*33:], row[:])
	}
	return a
}

// pp takes pr in [0,4095] and returns a refined 12-bit probability.
func (a *cmAPM) pp(pr, cx int) int {
	if pr < 0 {
		pr = 0
	} else if pr > 4095 {
		pr = 4095
	}
	s := int(stretchTab[pr]) + 2048 // 1..4095
	lo := s >> 7                     // 0..31
	w := s & 127
	base := cx*33 + lo
	if w >= 64 {
		a.idx = base + 1
	} else {
		a.idx = base
	}
	return (int(a.t[base])*(128-w) + int(a.t[base+1])*w) >> 11
}

func (a *cmAPM) update(y int) {
	g := y*65535 - int(a.t[a.idx])
	a.t[a.idx] = uint16(int(a.t[a.idx]) + (g >> 7))
}

const cmMinMatch = 5

// mixer updates are skipped when |error| (12-bit scale) is below this.
const cmSkipErr = 16

type cmMatch struct {
	tab   []int32
	ptr   int // index in hist of the predicted next byte
	mlen  int
	mbyte byte
	sm    [64]cmCtr
	smIdx int
}

func newCMMatch(capHint int) *cmMatch {
	sz := 1 << 18
	if capHint >= 1048576 {
		sz = 1 << 21
	} else if capHint >= 262144 {
		sz = 1 << 20
	}
	mm := &cmMatch{tab: make([]int32, sz), smIdx: -1}
	for i := range mm.sm {
		mm.sm[i] = cmCtrInit
	}
	return mm
}

func (mm *cmMatch) lenQ() int {
	if mm.mlen < 16 {
		return mm.mlen
	}
	q := 16 + (mm.mlen-16)>>2
	if q > 31 {
		q = 31
	}
	return q
}

// predict returns the two match inputs for the current bit.
func (mm *cmMatch) predict(c0, bitn int) (int, int) {
	mm.smIdx = -1
	if mm.mlen == 0 {
		return 0, 0
	}
	// the already coded bits of this byte must agree with the predicted byte
	if (int(mm.mbyte)|256)>>(8-uint(bitn)) != c0 {
		mm.mlen = 0
		return 0, 0
	}
	bit := int(mm.mbyte>>(7-uint(bitn))) & 1
	mm.smIdx = mm.lenQ()*2 + bit
	s := int(stretchTab[int(mm.sm[mm.smIdx]>>16)>>4])
	l := mm.mlen
	if l > 32 {
		l = 32
	}
	e := l * 32
	if bit == 0 {
		e = -e
	}
	return s, e
}

func (mm *cmMatch) update(y int) {
	if mm.smIdx >= 0 {
		mm.sm[mm.smIdx].upd(y, 255)
	}
}

// endByte is called after hist[n] has been appended.
func (mm *cmMatch) endByte(hist []byte, n int) {
	if mm.mlen > 0 {
		if mm.mbyte == hist[n] {
			mm.mlen++
			mm.ptr++
		} else {
			mm.mlen = 0
		}
	}
	if n >= cmMinMatch-1 {
		var h uint32
		for i := 0; i < cmMinMatch; i++ {
			h = (h + uint32(hist[n-i]) + 1) * 0x2F0B4D3F
		}
		slot := int((h ^ h>>15) & uint32(len(mm.tab)-1))
		if mm.mlen == 0 {
			cand := int(mm.tab[slot])
			if cand > 0 && cand <= n {
				l := 0
				for l < 64 && cand-1-l >= 0 && hist[cand-1-l] == hist[n-l] {
					l++
				}
				if l >= cmMinMatch {
					mm.mlen = l
					mm.ptr = cand
				}
			}
		}
		mm.tab[slot] = int32(n + 1)
	}
	if mm.mlen > 0 && mm.ptr <= n {
		mm.mbyte = hist[mm.ptr]
	} else {
		mm.mlen = 0
	}
}

type cm struct {
	d0 []cmCtr // order 0 direct
	d1 []cmCtr // order 1 direct
	h  [cmHashed][]cmCtr
	mm *cmMatch

	w  [3][]int32
	w2 []int32

	hist []byte
	c4   uint32
	c6   uint64
	wh   uint64 // current word hash
	pw   uint64 // previous word hash

	cx  [cmHashed]uint64
	bb  [cmHashed]int
	cp  [cmCtx]*cmCtr
	lim [cmCtx]int
	st  [cmNIn]int

	sel [3]int
	dj  [3]int
	pj  [3]int

	apm1 *cmAPM
	apm2 *cmAPM
	apm3 *cmAPM

	pr      int
	prMixer int
	c0      int
	bitn    int
	nib     int
}

func cmEntries(inputLen int) int {
	e := 1 << 16
	for e < inputLen*8 && e < 1<<22 {
		e <<= 1
	}
	return e
}

func newCM(capHint int) *cm {
	m := &cm{}
	m.d0 = make([]cmCtr, 256)
	m.d1 = make([]cmCtr, 1<<16)
	for i := range m.d0 {
		m.d0[i] = cmCtrInit
	}
	for i := range m.d1 {
		m.d1[i] = cmCtrInit
	}
	e := cmEntries(capHint)
	for i := 0; i < cmHashed; i++ {
		m.h[i] = make([]cmCtr, e)
	}
	m.lim = [cmCtx]int{255, 255, 255, 255, 255, 255, 255, 127, 127}
	m.mm = newCMMatch(capHint)
	m.w[0] = make([]int32, cmSetsA*cmNIn)
	m.w[1] = make([]int32, cmSetsB*cmNIn)
	m.w[2] = make([]int32, cmSetsC*cmNIn)
	for j := 0; j < 3; j++ {
		for i := range m.w[j] {
			m.w[j][i] = 6554 // ~0.1
		}
	}
	m.w2 = make([]int32, 256*3)
	for i := range m.w2 {
		m.w2[i] = 21845 // ~1/3
	}
	m.apm1 = newAPM(256)
	m.apm2 = newAPM(1 << 16)
	m.apm3 = newAPM(1 << 16)
	m.hist = make([]byte, 0, capHint)
	m.c0 = 1
	m.nib = 1
	m.byteCtx()
	m.buckets()
	return m
}

// byteCtx computes the per-byte context keys of the hashed models.
func (m *cm) byteCtx() {
	c4 := uint64(m.c4)
	c6 := m.c6
	var wk uint64
	wk = m.wh*0x2F0B4D3F + m.pw*0x1F3D5B79
	if m.wh == 0 {
		wk += (c4 & 0xFF) << 40
	}
	v := [cmHashed]uint64{
		c4 & 0xFFFF,
		c4 & 0xFFFFFF,
		c4,
		c6 & 0xFFFFFFFFFF,
		c6 & 0xFFFFFFFFFFFF,
		wk,
		(c4 >> 8) & 0xFFFF,
	}
	for i := 0; i < cmHashed; i++ {
		m.cx[i] = (v[i]+1)*0x9E3779B97F4A7C15 + uint64(i+1)*0xD6E8FEB86659FD93
	}
}

// lookup finds (or claims) the 16-slot bucket for hash h in table i.
func (m *cm) lookup(i int, h uint32) int {
	t := m.h[i]
	mask := uint32(len(t)>>4 - 1)
	chk := cmCtr(h | 1)
	b0 := int(h&mask) << 4
	if t[b0] == chk {
		return b0
	}
	b1 := b0 ^ 16
	if t[b1] == chk {
		return b1
	}
	v := b0
	if t[b0] != 0 {
		if t[b1] == 0 || t[b1+1]&0xFFFF < t[b0+1]&0xFFFF {
			v = b1
		}
	}
	t[v] = chk
	for k := 1; k < 16; k++ {
		t[v+k] = cmCtrInit
	}
	return v
}

// buckets selects the bucket of every hashed model (at nibble boundaries).
func (m *cm) buckets() {
	for i := 0; i < cmHashed; i++ {
		key := m.cx[i]
		if m.bitn == 4 {
			key += uint64(m.c0) * 0xA24BAED4963EE407
		}
		m.bb[i] = m.lookup(i, cmH(key))
	}
}

func (m *cm) predict() int {
	c1 := int(m.c4 & 0xFF)

	m.cp[0] = &m.d0[m.c0]
	m.cp[1] = &m.d1[c1<<8|m.c0]
	for i := 0; i < cmHashed; i++ {
		m.cp[2+i] = &m.h[i][m.bb[i]+m.nib]
	}
	for i := 0; i < cmCtx; i++ {
		a, b := cmIn(*m.cp[i])
		m.st[2*i] = a
		m.st[2*i+1] = b
	}
	a, b := m.mm.predict(m.c0, m.bitn)
	m.st[2*cmCtx] = a
	m.st[2*cmCtx+1] = b
	m.st[2*cmCtx+2] = 256

	lq := m.mm.mlen
	if lq > 15 {
		lq = 15
	}
	m.sel[0] = m.c0 * cmNIn
	m.sel[1] = (lq*8 + m.bitn) * cmNIn
	m.sel[2] = c1 * cmNIn

	// layer 1
	for j := 0; j < 3; j++ {
		w := m.w[j][m.sel[j] : m.sel[j]+cmNIn]
		var sum int64
		for k := 0; k < cmNIn; k++ {
			sum += int64(w[k]) * int64(m.st[k])
		}
		d := int(sum >> 16)
		if d > 2047 {
			d = 2047
		} else if d < -2047 {
			d = -2047
		}
		m.dj[j] = d
		p := squash(d)
		if p < 1 {
			p = 1
		} else if p > 4095 {
			p = 4095
		}
		m.pj[j] = p
	}

	// layer 2
	w2 := m.w2[m.c0*3 : m.c0*3+3]
	var sum2 int64
	for j := 0; j < 3; j++ {
		sum2 += int64(w2[j]) * int64(m.dj[j])
	}
	d := int(sum2 >> 16)
	if d > 2047 {
		d = 2047
	} else if d < -2047 {
		d = -2047
	}
	p := squash(d)
	if p < 1 {
		p = 1
	} else if p > 4095 {
		p = 4095
	}
	m.prMixer = p

	// SSE / APM stages
	p1 := m.apm1.pp(p, m.c0)
	p2 := m.apm2.pp(p, c1<<8|m.c0)
	p3 := m.apm3.pp(p, int(cmH(uint64(m.c4&0xFFFFFF)<<8|uint64(m.c0))>>16))
	p = (p*2 + p1 + p2*2 + p3*3) >> 3
	if p < 1 {
		p = 1
	} else if p > 4095 {
		p = 4095
	}
	m.pr = p
	return p
}

func (m *cm) update(y int) {
	// layer 1: every mixer trains on its OWN output
	for j := 0; j < 3; j++ {
		e := (y << 12) - m.pj[j]
		if e > -cmSkipErr && e < cmSkipErr {
			continue // already well predicted: skip the costly update
		}
		err := e * 10
		w := m.w[j][m.sel[j] : m.sel[j]+cmNIn]
		for k := 0; k < cmNIn; k++ {
			nw := w[k] + int32((int64(m.st[k])*int64(err))>>14)
			if nw > 1<<20 {
				nw = 1 << 20
			} else if nw < -(1 << 20) {
				nw = -(1 << 20)
			}
			w[k] = nw
		}
	}
	// layer 2 trains on the mixer output (before APM)
	e2 := (y << 12) - m.prMixer
	if e2 <= -cmSkipErr || e2 >= cmSkipErr {
		m.updLayer2(e2 * 6)
	}

	for i := 0; i < cmCtx; i++ {
		m.cp[i].upd(y, m.lim[i])
	}
	m.mm.update(y)
	m.apm1.update(y)
	m.apm2.update(y)
	m.apm3.update(y)
	m.advance(y)
}

func (m *cm) updLayer2(err2 int) {
	w2 := m.w2[m.c0*3 : m.c0*3+3]
	for j := 0; j < 3; j++ {
		nw := w2[j] + int32((int64(m.dj[j])*int64(err2))>>14)
		if nw > 1<<20 {
			nw = 1 << 20
		} else if nw < -(1 << 20) {
			nw = -(1 << 20)
		}
		w2[j] = nw
	}
}

func (m *cm) advance(y int) {
	m.c0 = m.c0<<1 | y
	m.nib = m.nib<<1 | y
	m.bitn++
	switch m.bitn {
	case 4:
		m.nib = 1
		m.buckets()
	case 8:
		b := byte(m.c0 & 0xFF)
		m.hist = append(m.hist, b)
		m.c4 = m.c4<<8 | uint32(b)
		m.c6 = (m.c6 << 8) | uint64(b)
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 128 {
			lc := b
			if b >= 'A' && b <= 'Z' {
				lc = b + 32
			}
			m.wh = (m.wh + uint64(lc) + 1) * 0x9E3779B1
		} else if m.wh != 0 {
			m.pw = m.wh
			m.wh = 0
		}
		m.mm.endByte(m.hist, len(m.hist)-1)
		m.c0 = 1
		m.bitn = 0
		m.nib = 1
		m.byteCtx()
		m.buckets()
	}
}

// cmEncode runs the arithmetic coder and hands every output byte to emit.
func cmEncode(raw []byte, emit func(byte)) {
	m := newCM(len(raw))
	x1, x2 := uint32(0), uint32(0xffffffff)
	for _, b := range raw {
		for i := 7; i >= 0; i-- {
			y := int(b>>uint(i)) & 1
			p := m.predict()
			xmid := x1 + uint32((uint64(x2-x1)*uint64(p))>>12)
			if y == 1 {
				x2 = xmid
			} else {
				x1 = xmid + 1
			}
			m.update(y)
			for (x1^x2)&0xff000000 == 0 {
				emit(byte(x2 >> 24))
				x1 <<= 8
				x2 = x2<<8 | 255
			}
		}
	}
	emit(byte(x1 >> 24))
	emit(byte(x1 >> 16))
	emit(byte(x1 >> 8))
	emit(byte(x1))
}

func cmHeader(n int) []byte {
	var out bytes.Buffer
	out.WriteString("CMX1")
	var tmp [binary.MaxVarintLen64]byte
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(n))])
	return out.Bytes()
}

func cmCompress(raw []byte) []byte {
	out := bytes.NewBuffer(cmHeader(len(raw)))
	cmEncode(raw, func(b byte) { out.WriteByte(b) })
	return out.Bytes()
}

// cmDecodeCore decodes n bytes, pulling coder bytes from next().
func cmDecodeCore(n int, next func() uint32) []byte {
	x1, x2 := uint32(0), uint32(0xffffffff)
	var x uint32
	for i := 0; i < 4; i++ {
		x = x<<8 | next()
	}
	m := newCM(n)
	for len(m.hist) < n {
		for bit := 0; bit < 8; bit++ {
			p := m.predict()
			xmid := x1 + uint32((uint64(x2-x1)*uint64(p))>>12)
			y := 0
			if x <= xmid {
				y = 1
				x2 = xmid
			} else {
				x1 = xmid + 1
			}
			m.update(y)
			for (x1^x2)&0xff000000 == 0 {
				x1 <<= 8
				x2 = x2<<8 | 255
				x = x<<8 | next()
			}
		}
	}
	return m.hist
}

func cmDecompress(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("CMX1")) {
		return nil, errors.New("not CMX1 data")
	}
	n64, k := binary.Uvarint(comp[4:])
	if k <= 0 || n64 > 1<<31 {
		return nil, errors.New("CMX1 header corrupted")
	}
	in := comp[4+k:]
	pos := 0
	next := func() uint32 {
		if pos < len(in) {
			b := in[pos]
			pos++
			return uint32(b)
		}
		return 0
	}
	return cmDecodeCore(int(n64), next), nil
}

// cmPipe lets the verifying decoder consume bytes while the encoder is
// still producing them, so verification costs no extra wall-clock time.
type cmPipe struct {
	mu   sync.Mutex
	cv   *sync.Cond
	buf  []byte
	done bool
}

func newCmPipe() *cmPipe {
	p := &cmPipe{}
	p.cv = sync.NewCond(&p.mu)
	return p
}

func (p *cmPipe) put(chunk []byte) {
	p.mu.Lock()
	p.buf = append(p.buf, chunk...)
	p.mu.Unlock()
	p.cv.Broadcast()
}

func (p *cmPipe) finish() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
	p.cv.Broadcast()
}

func (p *cmPipe) get(pos int) (byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for pos >= len(p.buf) && !p.done {
		p.cv.Wait()
	}
	if pos < len(p.buf) {
		return p.buf[pos], true
	}
	return 0, false
}

// cmCompressVerified compresses raw and, in parallel, decodes the stream
// as it is produced. ok reports whether the round trip reproduced raw.
func cmCompressVerified(raw []byte) (comp []byte, ok bool) {
	out := bytes.NewBuffer(cmHeader(len(raw)))
	p := newCmPipe()
	res := make(chan bool, 1)
	go func() {
		pos := 0
		next := func() uint32 {
			b, got := p.get(pos)
			if got {
				pos++
				return uint32(b)
			}
			return 0
		}
		res <- bytes.Equal(cmDecodeCore(len(raw), next), raw)
	}()
	pend := make([]byte, 0, 64)
	cmEncode(raw, func(b byte) {
		out.WriteByte(b)
		pend = append(pend, b)
		if len(pend) >= 64 {
			p.put(pend)
			pend = pend[:0]
		}
	})
	if len(pend) > 0 {
		p.put(pend)
	}
	p.finish()
	ok = <-res
	return out.Bytes(), ok
}

func cliReport() func(map[string]any) {
	var mu sync.Mutex
	return func(e map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		switch e["type"] {
		case "algo":
			if e["skipped"] != nil {
				reason, _ := e["reason"].(string)
				msg := "skipped"
				switch reason {
				case "too_large":
					msg = "skipped (file too large)"
				case "incompressible":
					msg = "skipped (incompressible)"
				case "use_parallel":
					msg = "skipped (using lzw-parallel)"
				case "use_single":
					msg = "skipped (using lzw)"
				case "not_dna":
					msg = "skipped (not DNA)"
				}
				fmt.Fprintf(os.Stderr, "  %-13v %s\n", e["name"], msg)
			} else if e["failed"] != nil {
				fmt.Fprintf(os.Stderr, "  %-13v failed\n", e["name"])
			} else {
				fmt.Fprintf(os.Stderr, "  %-13v %v bytes (%v ms)\n", e["name"], e["size"], e["ms"])
			}
		case "pack":
			orig, _ := e["orig"].(int)
			packed, _ := e["packed"].(int)
			pc := 0.0
			if orig > 0 {
				pc = float64(packed) / float64(orig) * 100
			}
			fmt.Fprintf(os.Stderr, "Compressed: %d -> %d bytes (%.1f%%) with %v\n", orig, packed, pc, e["algo"])
		case "packstart":
			if pc, _ := e["precompressed"].(bool); pc {
				fmt.Fprintf(os.Stderr, "[info] precompressed file detected — slow algorithms skipped\n")
			}
		case "autosize":
			fmt.Fprintf(os.Stderr, "[auto-size] size: %v → %v (modules=%v, target %vpx/module)\n",
				e["from"], e["to"], e["modules"], e["target_px"])
		}
	}
}
