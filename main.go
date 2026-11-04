// pixpack - encode text, links, images, or any binary file into images
// (pixel PNG / color grid / QR) and decode them back.
//
// Install:  go mod tidy && go build -o pixpack
//
// Encode:
//   ./pixpack text "Hello world" -o hello.png
//   ./pixpack url example.com -o link.png
//   ./pixpack image photo.jpg -o p.png          # lossless, auto-split into multiple QRs if large
//   ./pixpack image photo.jpg -fit -o p.png     # shrink + JPEG compress to fit 1 QR
//   ./pixpack file data.bin -o d.png            # any binary, compressed (gzip/zstd/lzw/xz/brotli), auto-split
//   ./pixpack file data.bin -o d.png            # default -fmt pixel: write directly into 16-bit RGBA PNG pixels (8 bytes/px)
//   ./pixpack file data.bin -fmt grid|qr ...    # color grid / QR (scannable by phone)
// Decode (restore original file from QR images):
//   ./pixpack decode -o out/ d_*.png
// Web server (port 8090):
//   ./pixpack serve [-addr :8090]
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

// ---------- Packaging: [name][data] -> best algo -> base45 -> chunk ----------

// pack = packWith without progress reporting.
func pack(name string, data []byte) ([]byte, error) { return packWith(name, data, nil) }

// precompressedExt are file extensions that are already compressed inside.
// Running brotli/CMX1/xz on them wastes minutes for <10% savings.
var precompressedExt = map[string]bool{
	// Video/audio
	".3gp": true, ".mp4": true, ".mkv": true, ".avi": true, ".mov": true,
	".webm": true, ".flv": true, ".wmv": true, ".m4v": true, ".mpg": true,
	".mp3": true, ".aac": true, ".ogg": true, ".flac": true, ".m4a": true,
	".opus": true, ".wma": true, ".wav": true, ".aiff": true,
	// Images (already compressed internally)
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".heic": true, ".heif": true, ".avif": true, ".bmp": true, ".tiff": true,
	".tif": true, ".ico": true,
	// Archives / packages
	".zip": true, ".apk": true, ".jar": true, ".aar": true, ".war": true,
	".rar": true, ".7z": true, ".gz": true, ".bz2": true, ".xz": true,
	".zst": true, ".lz4": true, ".lzma": true, ".tgz": true, ".tbz2": true,
	".txz": true, ".tar.gz": true, ".tar.bz2": true, ".tar.xz": true,
	".cab": true, ".deb": true, ".rpm": true, ".dmg": true, ".iso": true,
	".whl": true, ".egg": true,
	// Documents (Office Open XML = ZIP container)
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true, ".epub": true, ".mobi": true,
	// Executables (compiled, may contain compressed sections)
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true,
	".class": true, ".dex": true, ".wasm": true,
}

// precompressedMagic detects container formats by their magic bytes,
// even if the extension was renamed.
var precompressedMagic = [][]byte{
	{0x1F, 0x8B},             // gzip
	{0x50, 0x4B, 0x03, 0x04}, // ZIP / APK / JAR / DOCX / EPUB
	{0x50, 0x4B, 0x05, 0x06}, // ZIP empty
	{0x50, 0x4B, 0x07, 0x08}, // ZIP split
	{0x89, 0x50, 0x4E, 0x47}, // PNG
	{0xFF, 0xD8, 0xFF},       // JPEG
	{0x47, 0x49, 0x46, 0x38}, // GIF8
	{0x66, 0x74, 0x79, 0x70}, // "ftyp" — MP4 / 3GP / MOV / M4A
	{0x49, 0x44, 0x33},       // "ID3" — MP3
	{0x4F, 0x67, 0x67, 0x53}, // "OggS" — OGG / OPUS
	{0x66, 0x4C, 0x61, 0x43}, // "fLaC" — FLAC
	{0x25, 0x50, 0x44, 0x46}, // "%PDF"
	{0x52, 0x61, 0x72, 0x21}, // "Rar!"
	{0x37, 0x7A, 0xBC, 0xAF}, // 7z
	{0xFD, '7', 'z', 'X', 'Z'}, // xz
	{0x28, 0xB5, 0x2F, 0xFD}, // zstd
	{0x04, 0x22, 0x4D, 0x18}, // lz4
	{0x1A, 0x45, 0xDF, 0xA3}, // matroska / webm
}

// looksPrecompressed returns true if the file is likely already compressed
// (image/audio/video/archive/office doc) based on extension or magic bytes.
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

// packWith concatenates [name][data] then tries multiple compression algorithms
// in parallel and keeps the smallest.
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
	total := 7
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
	} else if len(raw) >= lzwParallelMinSize {
		run("lzw-parallel", func() []byte {
			return lzwCompressParallel(raw)
		})
		emit(map[string]any{"type": "algo", "name": "lzw", "skipped": true, "reason": "use_parallel"})
	} else {
		run("lzw", func() []byte {
			return lzwCompress(raw)
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
			c := cmCompress(raw)
			if len(raw) <= 256<<10 {
				if d, err := cmDecompress(c); err != nil || !bytes.Equal(d, raw) {
					return nil
				}
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

// unpack returns filename, data, and the name of the compression algorithm used.
func unpack(comp []byte) (string, []byte, string, error) {
	var raw []byte
	var err error
	algo := "gzip"
	switch {
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

// ---------- Image shrink to fit one QR ----------

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

func generate(mode, text, name string, data []byte, o opts) ([][]byte, error) {
	encode := func(s string) ([]byte, error) {
		b, err := qrcode.Encode(s, o.RL, o.Size)
		if err != nil {
			return nil, fmt.Errorf("cannot create QR (content too large?): %w", err)
		}
		return b, nil
	}
	one := func(s string) ([][]byte, error) {
		b, err := encode(s)
		if err != nil {
			return nil, err
		}
		return [][]byte{b}, nil
	}

	switch mode {
	case "text":
		if text == "" {
			return nil, errors.New("content is empty")
		}
		return one(text)
	case "url":
		u, err := normalizeURL(text)
		if err != nil {
			return nil, err
		}
		return one(u)
	case "image":
		if len(data) == 0 {
			return nil, errors.New("no image provided")
		}
		if o.Fit {
			uri, err := fitImage(data, maxDirectLen)
			if err != nil {
				return nil, err
			}
			return one(uri)
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
  pixpack <text|url|image|file> <content|path> [-o out.png] [-s 768] [-l M] [-c 900] [-fit]
                [-fmt pixel|grid|qr] [-depth 16 -ch 4 -px 512] [-bits 2 -cell 3 -grid 256]
  pixpack decode [-o dir] img1.png img2.png ...
  pixpack serve [-addr :8090]`)
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
		Depth: normDepth(*depth), Channels: normCh(*chn), PixSide: clampInt(*px, 16, 4096)}
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
	}
	if n, err := strconv.Atoi(get("chunk")); err == nil && n >= 100 && n <= 2000 {
		o.Chunk = n
	}
	return o
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
			if mode == "text" || mode == "url" || (mode == "image" && oo.Fit) || eff == "" {
				eff = "qr"
			}
			inBytes := len(data)
			if mode == "text" || mode == "url" {
				inBytes = len(r.FormValue("text"))
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
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
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
</style></head><body>
<h2>Encode → Image</h2>
<label>Mode</label>
<select id="mode">
<option value="text">Text</option>
<option value="url">URL / Link</option>
<option value="image">Image</option>
<option value="file">Any binary file</option>
</select>
<div id="textBox"><label>Content</label><textarea id="text" rows="4"></textarea></div>
<div id="fileBox" style="display:none"><label>Choose file</label><input type="file" id="file"><small id="finfo"></small></div>
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
<div><label>Image size (px)</label><input id="size" type="number" value="768" min="128" max="2048"></div>
<div><label>Error correction</label><select id="level"><option>L</option><option selected>M</option><option>Q</option><option>H</option></select></div>
</div>
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
  $("textBox").style.display=f?"none":"";
  $("fitBox").style.display=(m==="image")?"":"none";
  $("fmtBox").style.display=f?"":"none";
  $("gridBox").style.display=(f&&$("format").value==="grid")?"":"none";
  $("pixBox").style.display=(f&&$("format").value==="pixel")?"":"none";
}
$("mode").onchange=sync;$("format").onchange=sync;sync();
$("file").onchange=function(){var f=this.files[0];$("finfo").textContent=f?(f.name+" — "+fmt(f.size)):"";};
$("dfiles").onchange=function(){var t=0;for(var i=0;i<this.files.length;i++)t+=this.files[i].size;$("dfinfo").textContent=this.files.length?(this.files.length+" image(s) — "+fmt(t)):"";};
function save(blob,name){var a=document.createElement("a");a.href=URL.createObjectURL(blob);a.download=name;document.body.appendChild(a);a.click();a.remove();}
function build(){
  var fd=new FormData();
  fd.append("mode",$("mode").value);fd.append("text",$("text").value);
  fd.append("size",$("size").value);fd.append("level",$("level").value);
  fd.append("fit",$("fit").checked?"1":"0");
  fd.append("format",$("format").value);fd.append("bits",$("bits").value);
  fd.append("cell",$("cell").value);fd.append("gridw",$("gridw").value);
  fd.append("depth",$("depth").value);fd.append("ch",$("chn").value);fd.append("px",$("px").value);
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

function skipLabel(r){
  switch(r.reason){
    case "too_large": return "skipped (file too large)";
    case "incompressible": return "skipped (incompressible data)";
    case "use_parallel": return "skipped (using lzw-parallel instead)";
    case "use_single": return "skipped (using lzw instead)";
    case "precompressed": return "skipped (already compressed content)";
  }
  return "skipped";
}

function render(d,rows,pack){
  var h="";
  if(d.precompressed){
    h+="<div class=\"notice\" style=\"background:#fef3c7;border:1px solid #f59e0b;color:#78350f;padding:10px;border-radius:8px;margin-bottom:12px\">";
    h+="⚠️ <b>Already-compressed file detected</b> (video/audio/image/archive/office). ";
    h+="Slow algorithms (xz, brotli, CMX1) were skipped — they only save &lt;10% but cost minutes of CPU. ";
    h+="Only fast algorithms (gzip, zstd, lzw) were tried.";
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
  var fd=build();fd.append("stream","1");
  var xhr=new XMLHttpRequest();xhr.open("POST","/api/encode");
  var pos=0,total=0,doneN=0,rows=[],pack=null,finished=false;
  showProg("eprog",true);setBar("e",0,"Uploading…","");
  xhr.upload.onprogress=function(e){if(e.lengthComputable)setBar("e",e.loaded/e.total*100,"Uploading",fmt(e.loaded)+" / "+fmt(e.total));};
  xhr.upload.onload=function(){setBar("e",0,"Server is processing…","");};
  function handle(e){
    if(e.error){$("err").textContent=e.error;showProg("eprog",false);finished=true;return;}
    if(e.type==="stage")setBar("e",0,e.msg,"");
    else if(e.type==="packstart"){total=e.total;doneN=0;rows=[];setBar("e",0,"Compressing","0/"+total+" algorithms");}
    else if(e.type==="algo"){
      rows.push(e);
      doneN++;
      var lbl=e.skipped?" (skipped)":(e.failed?" (failed)":"");
      setBar("e",doneN/total*100,"Compressing",doneN+"/"+total+" — "+e.name+lbl);
    }
    else if(e.type==="pack")pack=e;
    else if(e.type==="progress")setBar("e",e.done/e.total*100,"Creating images",e.done+"/"+e.total);
    else if(e.type==="done"){finished=true;setBar("e",100,"Done",(e.ms/1000).toFixed(2)+" s");render(e,rows,pack);}
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
    if(ct.indexOf("text/plain")===0){
      var fr2=new FileReader();fr2.onload=function(){var p=document.createElement("pre");p.textContent=fr2.result;$("dout").appendChild(p);};fr2.readAsText(xhr.response);
      return;
    }
    if(ct.indexOf("image/")===0){var im=new Image();im.src=URL.createObjectURL(xhr.response);im.style.maxWidth="100%";im.style.marginTop="8px";$("dout").appendChild(im);}
    var a=document.createElement("a");a.href=URL.createObjectURL(xhr.response);a.download=info.name||"decoded.bin";a.textContent="Download "+(info.name||"decoded.bin");a.style.display="block";a.style.marginTop="8px";$("dout").appendChild(a);
  };
  xhr.send(fd);
};
</script></body></html>`

// ======================================================================
// Color grid format (grid)
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
// Pixel format (densest)
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
// Formats:
//   "LZW1" + uvarint(len) + bitstream            ← regular (with run codes)
//   "LZWS" + uvarint(len) + raw bytes            ← store mode
//   "LZWP" + uvarint(total) + uvarint(n) + ...   ← parallel

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

// lzwLooksIncompressible samples 4 regions (start, 1/4, 1/2, 3/4) of 512 bytes
// each. Container formats (.3gp, .mp4, .apk) have structured 1KB headers but
// entropy-coded bulk — sampling multiple regions catches them.
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

// lzwStoreMode wraps raw bytes in a store-mode container ("LZWS").
func lzwStoreMode(raw []byte) []byte {
	out := make([]byte, 0, 5+len(raw)+10)
	out = append(out, 'L', 'Z', 'W', 'S')
	var tmp [binary.MaxVarintLen64]byte
	out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))]...)
	out = append(out, raw...)
	return out
}

// lzwShouldStore returns true if compressed output should be replaced by store
// mode. Rule: compressed must save at least 5% AND be smaller than storeLen.
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

	// Whole-file store-mode fallback: if parallel output isn't clearly smaller
	// than input, emit store mode instead of a bloated LZWP container.
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
// CMX1 v2 - context mixing (lpaq-style)
// ======================================================================

var stretchTab [4096]int16

func squash(d int) int {
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

func init() {
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
}

const (
	cmOrders = 6
	cmNIn    = cmOrders + 2
	cmNCtx   = 256
)

type cmCtr uint32

func (c *cmCtr) pr() int { return int(*c >> 16) }

func (c *cmCtr) upd(y int) {
	n := int(*c & 0xFFFF)
	p := int(*c >> 16)
	rate := n + 2
	if rate > 32 {
		rate = 32
	}
	p += (y*65535 - p) / rate
	if p < 1 {
		p = 1
	} else if p > 65534 {
		p = 65534
	}
	if n < 1023 {
		n++
	}
	*c = cmCtr(uint32(p)<<16 | uint32(n))
}

type cmMatch struct {
	tab   []int32
	mptr  int
	mlen  int
	mbyte byte
	bitn  int
}

func cmTableSize(inputLen int) int {
	switch {
	case inputLen < 65536:
		return 1 << 15
	case inputLen < 262144:
		return 1 << 17
	case inputLen < 1048576:
		return 1 << 19
	default:
		return 1 << 21
	}
}

func newCMMatch(capHint int) *cmMatch {
	sz := 1 << 18
	if capHint >= 1048576 {
		sz = 1 << 20
	} else if capHint >= 262144 {
		sz = 1 << 19
	}
	return &cmMatch{tab: make([]int32, sz)}
}

func (mm *cmMatch) predict() int {
	if mm.mlen == 0 {
		return 0
	}
	bit := int(mm.mbyte>>(7-uint(mm.bitn))) & 1
	conf := 400
	if mm.mlen < 8 {
		conf = 100 + mm.mlen*40
	}
	if bit == 1 {
		return conf
	}
	return -conf
}

func (mm *cmMatch) endByte(hist []byte, n int) {
	if mm.mlen > 0 {
		if mm.mbyte == hist[n] {
			mm.mlen++
			mm.mptr++
		} else {
			mm.mlen = 0
		}
	}

	if n >= 4 {
		var h uint32
		for i := 0; i < 4; i++ {
			h = h*0x9E3779B1 + uint32(hist[n-3+i])
		}
		slot := int(h & uint32(len(mm.tab)-1))
		prev := int(mm.tab[slot])
		if mm.mlen == 0 && prev >= 4 && prev <= n {
			ok := true
			for i := 0; i < 4; i++ {
				if hist[prev-4+i] != hist[n-3+i] {
					ok = false
					break
				}
			}
			if ok {
				mm.mptr = prev
				mm.mlen = 4
			}
		}
		mm.tab[slot] = int32(n + 1)
	}

	if mm.mlen > 0 && mm.mptr < len(hist) {
		mm.mbyte = hist[mm.mptr]
	} else {
		mm.mlen = 0
	}
	mm.bitn = 0
}

type cm struct {
	t   [cmOrders][]cmCtr
	mm  *cmMatch
	w   []int32

	hist []byte
	c4   uint32
	c6   uint64

	hs  [cmOrders]uint32
	idx [cmOrders]int
	st  [cmNIn]int

	mxc  int
	pr   int
	c0   int
	bitn int
}

func newCM(capHint int) *cm {
	m := &cm{}
	sz := cmTableSize(capHint)
	sizes := [cmOrders]int{256, 1 << 16, sz, sz, sz, sz}
	for i := 0; i < cmOrders; i++ {
		m.t[i] = make([]cmCtr, sizes[i])
		init := cmCtr(uint32(32768) << 16)
		for j := range m.t[i] {
			m.t[i][j] = init
		}
	}
	m.mm = newCMMatch(capHint)
	m.w = make([]int32, cmNCtx*cmNIn)
	for i := range m.w {
		m.w[i] = 1 << 14
	}
	m.hist = make([]byte, 0, capHint)
	m.c0 = 1
	return m
}

func (m *cm) predict() int {
	m.idx[0] = m.c0
	m.st[0] = int(stretchTab[m.t[0][m.c0].pr()>>4])

	i1 := int(m.c4&0xFF)<<8 | m.c0
	m.idx[1] = i1
	m.st[1] = int(stretchTab[m.t[1][i1].pr()>>4])

	c0u := uint32(m.c0) * 0x9E3779B1
	h2 := ((m.c4 & 0xFFFF) + c0u) * 0x85EBCA6B
	m.idx[2] = int(h2 & uint32(len(m.t[2])-1))
	m.st[2] = int(stretchTab[m.t[2][m.idx[2]].pr()>>4])
	h3 := ((m.c4 & 0xFFFFFF) + c0u*2) * 0xC2B2AE35
	m.idx[3] = int(h3 & uint32(len(m.t[3])-1))
	m.st[3] = int(stretchTab[m.t[3][m.idx[3]].pr()>>4])
	h4 := (m.c4 + c0u*3) * 0x6F4F2A1D
	m.idx[4] = int(h4 & uint32(len(m.t[4])-1))
	m.st[4] = int(stretchTab[m.t[4][m.idx[4]].pr()>>4])
	h6 := uint32((m.c6 + uint64(c0u)*4) * 0x9E3779B97F4A7C15 >> 32)
	m.idx[5] = int(h6 & uint32(len(m.t[5])-1))
	m.st[5] = int(stretchTab[m.t[5][m.idx[5]].pr()>>4])

	m.st[cmOrders] = m.mm.predict()
	m.st[cmOrders+1] = 256

	m.mxc = m.c0 * cmNIn
	var sum int64
	for k := 0; k < cmNIn; k++ {
		sum += int64(m.w[m.mxc+k]) * int64(m.st[k])
	}
	d := int(sum >> 16)
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
	m.pr = p
	return p
}

func (m *cm) update(y int) {
	err := ((y << 12) - m.pr) * 12
	for k := 0; k < cmNIn; k++ {
		nw := m.w[m.mxc+k] + int32((int64(m.st[k])*int64(err))>>14)
		if nw > 1<<20 {
			nw = 1 << 20
		} else if nw < -(1 << 20) {
			nw = -(1 << 20)
		}
		m.w[m.mxc+k] = nw
	}

	for i := 0; i < cmOrders; i++ {
		m.t[i][m.idx[i]].upd(y)
	}

	m.c0 = m.c0<<1 | y
	m.bitn++
	if m.mm.mlen > 0 {
		m.mm.bitn++
	}
	if m.bitn == 8 {
		b := byte(m.c0 & 0xFF)
		m.hist = append(m.hist, b)
		m.c4 = m.c4<<8 | uint32(b)
		m.c6 = (m.c6 << 8) | uint64(b)
		m.mm.endByte(m.hist, len(m.hist)-1)
		m.c0 = 1
		m.bitn = 0
	}
}

func cmCompress(raw []byte) []byte {
	m := newCM(len(raw))
	var out bytes.Buffer
	out.WriteString("CMX1")
	var tmp [binary.MaxVarintLen64]byte
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(raw)))])
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
				out.WriteByte(byte(x2 >> 24))
				x1 <<= 8
				x2 = x2<<8 | 255
			}
		}
	}
	out.WriteByte(byte(x1 >> 24))
	out.WriteByte(byte(x1 >> 16))
	out.WriteByte(byte(x1 >> 8))
	out.WriteByte(byte(x1))
	return out.Bytes()
}

func cmDecompress(comp []byte) ([]byte, error) {
	if !bytes.HasPrefix(comp, []byte("CMX1")) {
		return nil, errors.New("not CMX1 data")
	}
	n64, k := binary.Uvarint(comp[4:])
	if k <= 0 || n64 > 1<<31 {
		return nil, errors.New("CMX1 header corrupted")
	}
	n := int(n64)
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
	return m.hist, nil
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
		}
	}
}
