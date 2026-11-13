# Go JPEG XL libraries, side by side

A separate module (so gojxl itself depends on none of them) that times the
Go JPEG XL libraries on the same files:

- [gojxl](https://github.com/bdrtr/gojxl) (this repository, pure Go)
- [gen2brain/jxl](https://github.com/gen2brain/jxl) (Go port of jxl-rs, with SIMD assembly)
- [gen2brain/jpegxl](https://github.com/gen2brain/jpegxl) (libjxl compiled to WASM, run by wazero)
- [kpfaulkner/jxl-go](https://github.com/kpfaulkner/jxl-go) (pure Go, decoder only)

Build with `-tags nodynamic`, else gen2brain/jpegxl uses a system libjxl
(through purego) instead of its WASM build:

```sh
go build -tags nodynamic -o rivals .
./rivals decode   gojxl|gen2brain-jxl|jpegxl-wasm|jxl-go  file.jxl  RUNS
./rivals encode   gojxl|gen2brain-jxl|jpegxl-wasm  image.png  QUALITY  RUNS
./rivals lossless gojxl|gen2brain-jxl|jpegxl-wasm  image.png  RUNS
```

Each prints the best wall time of RUNS runs (GOMAXPROCS sets the threads)
and writes its output (`out_<lib>.png`, `enc_<lib>.jxl`), so quality can be
checked with `gojxl compare`.
