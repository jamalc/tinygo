.PHONY: clock docks conway stress gravity bench bloom marble breakout acceldiag suspendtest teartest psramprobe sizes sizes-one fetch serve simulate clean

# BOARD is one of our friendly board names; it resolves to a tinygo TARGET and the WiFi
# backend tag the clock needs. The HUB75 engine is selected by the target's chip tag
# (matrixportal-m4 => atsamd51 => hub75_samd51_*.go, pico2-w => rp2350 => hub75_rp2350.go,
# esp32s3-generic => esp32s3 => hub75_esp32s3.go; see internal/hub75). DRIVER below overrides
# the chip default: DRIVER=isr (M4 only). The M4 and
# Interstate targets bake in their WiFi backends (ninafw / cyw43439);
# esp32s3-generic does not, so the S3 maps in `espradio` here. An unrecognized BOARD passes
# straight through as the target (raw tinygo target names still work), with no WiFi tag.
BOARD ?= matrixportal-m4
ifeq ($(BOARD),matrixportal-m4)
  TARGET := matrixportal-m4
  WIFITAG :=
else ifeq ($(BOARD),matrixportal-s3)
  TARGET := esp32s3-generic
  WIFITAG := espradio
else ifeq ($(BOARD),interstate-75w)
  TARGET := pico2-w
  WIFITAG :=
else
  TARGET := $(BOARD)
  WIFITAG :=
endif

# BOARDS is the set the `sizes` target sweeps to compare driver footprints across chips.
BOARDS ?= matrixportal-m4 matrixportal-s3 interstate-75w

# DRIVER picks a non-default engine. Leave it empty for the chip's default (evsys / pio / lcd).
# DRIVER=isr selects the M4's ISR-conductor engine (=> -tags=atsamd51_isr, M4 only) — the only
# alternative there is, now that every engine is silicon-offloaded.
DRIVER ?=
ifeq ($(DRIVER),isr)
  DRIVERTAG := atsamd51_isr
endif

# Build tags = the WiFi backend + the engine selector + any ad-hoc TAGS, joined on commas with
# empties dropped.
TAGS ?=
comma := ,
space := $(empty) $(empty)
hub75tags = $(subst $(space),$(comma),$(strip $(WIFITAG) $(DRIVERTAG) $(TAGS)))

# TODO: https://tinygo.org/docs/guides/optimizing-binaries/
# The games (marble, breakout) are steered by the on-board accelerometer; breakout
# prefers a seesaw rotary encoder on the STEMMA QT / Qw/ST port when one answers at
# boot (internal/encoder). acceldiag prints the accelerometer's axes so internal/accel's
# per-board orientation can be set. Boards without either still build and run the
# games, just sitting level.
clock docks conway stress gravity bench bloom marble breakout acceldiag suspendtest teartest psramprobe:
	tinygo flash -monitor -target=$(TARGET) -stack-size=8KB -tags=$(hub75tags) ./cmd/$@

# Static memory segments per chip's driver (host-side; complements cmd/bench). One driver
# per chip now, so the comparison is across BOARDS. Uses cmd/conway (no WiFi) to isolate the
# HUB75 driver footprint.
sizes:
	@for b in $(BOARDS); do \
		echo "== $$b =="; \
		$(MAKE) --no-print-directory sizes-one BOARD=$$b; \
	done

sizes-one:
	@tinygo build -size short -o /tmp/sc-size.uf2 -target=$(TARGET) -stack-size=8KB ./cmd/stress 2>&1 | tail -2

# Build the GTFS fetcher binary for deployment
fetch:
	GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/fetch

# Build the service binary for deployment
serve:
	GOOS=linux GOARCH=amd64 go build -o bin/ ./cmd/serve

# Build the desktop simulator
simulate:
	go build -o bin/ ./cmd/simulate

clean:
	rm -rf ~/Library/Caches/tinygo
	rm -rf ./bin

echo:
	echo $(VALUE)
