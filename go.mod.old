module simple-cot-chat

go 1.26.0

require (
	codeberg.org/readeck/go-readability/v2 v2.1.2
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.2
	github.com/duckdb/duckdb-go/v2 v2.4.3
	github.com/google/uuid v1.6.0
	github.com/ollama/ollama v0.30.10
	github.com/sugarme/tokenizer v0.3.0
	github.com/wailsapp/wails/v2 v2.12.0
	github.com/yalue/onnxruntime_go v1.19.0
	golang.org/x/net v0.55.0
)

require (
	git.sr.ht/~jackmordaunt/go-toast/v2 v2.0.3 // indirect
	github.com/JohannesKaufmann/dom v0.3.1 // indirect
	github.com/andybalholm/cascadia v1.3.4 // indirect
	github.com/apache/arrow-go/v18 v18.5.1 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/buger/jsonparser v1.1.1 // indirect
	github.com/duckdb/duckdb-go-bindings v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/windows-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go/arrowmapping v0.0.22 // indirect
	github.com/duckdb/duckdb-go/mapping v0.0.22 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-shiori/dom v0.0.0-20230515143342-73569d674e1c // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogs/chardet v0.0.0-20211120154057-b7413eaefb8f // indirect
	github.com/google/flatbuffers v25.12.19+incompatible // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/itlightning/dateparse v0.2.1 // indirect
	github.com/jchv/go-winloader v0.0.0-20210711035445-715c2860da7e // indirect
	github.com/klauspost/compress v1.18.3 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/labstack/echo/v4 v4.13.3 // indirect
	github.com/labstack/gommon v0.4.2 // indirect
	github.com/leaanthony/go-ansi-parser v1.6.1 // indirect
	github.com/leaanthony/gosod v1.0.4 // indirect
	github.com/leaanthony/slicer v1.6.0 // indirect
	github.com/leaanthony/u v1.1.1 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/patrickmn/go-cache v2.1.0+incompatible // indirect
	github.com/pierrec/lz4/v4 v4.1.25 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/samber/lo v1.49.1 // indirect
	github.com/schollz/progressbar/v2 v2.15.0 // indirect
	github.com/sugarme/regexpset v0.0.0-20200920021344-4d4ec8eaf93c // indirect
	github.com/tkrajina/go-reflector v0.5.8 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/wailsapp/go-webview2 v1.0.22 // indirect
	github.com/wailsapp/mimetype v1.4.1 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/exp v0.0.0-20260112195511-716be5621a96 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/telemetry v0.0.0-20260409153401-be6f6cb8b1fa // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// replace github.com/wailsapp/wails/v2 v2.12.0 => /home/config/go/pkg/mod

// Pin DuckDB to the 1.4.x LTS engine.
//
// A `require` alone does not hold. DuckDB encodes the engine version into the module
// version — v2.10505.0 is DuckDB 1.5.5 — so under minimal version selection the 1.5.x
// line is *numerically greater* than the 1.4.x line (10505 > 4) and always wins. A
// `go mod tidy` or `go get -u` therefore silently upgrades the engine, which is exactly
// how DuckDB 1.5.5 got in unnoticed.
//
// 1.5.5 does not work under Wails on Linux: it installs a SIGSEGV handler without
// SA_ONSTACK, and Go aborts with "non-Go code set up signal handler without SA_ONSTACK
// flag" during sql.Open. TestDuckDBVersionIsPinned fails loudly if this pin is lost.
replace github.com/duckdb/duckdb-go/v2 => github.com/duckdb/duckdb-go/v2 v2.4.3
