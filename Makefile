.PHONY: all rcpud l9term drawsrv tlssrv_o9 clean test install

DT=/home/scott/Repo/go-dp9ik/drawterm
DT_REAL=/home/scott/Repo/drawterm
INCS=-I$(DT)/include -I$(DT)
LIBS=-Wl,--start-group $(DT)/libsec/libsec.a $(DT)/libauthsrv/libauthsrv.a $(DT)/libmp/libmp.a $(DT)/libc/libc.a $(DT)/libmachdep.a -Wl,--end-group -lm -lpthread

all: rcpud l9term drawsrv tlssrv_o9

rcpud:
	@echo "Building rcpud..."
	@mkdir -p bin
	go build -tags rcpud -o bin/rcpud .

l9term:
	@echo "Building l9term..."
	@mkdir -p bin
	go build -tags l9term -o bin/l9term .

drawsrv:
	@echo "Building drawsrv..."
	@mkdir -p bin
	cd o9draw && go build -tags drawsrv -o ../bin/drawsrv .

tlssrv_o9: tlssrv_o9.c stubs_o9.c
	@echo "Building tlssrv_o9..."
	gcc -O2 $(INCS) -o bin/tlssrv_o9 tlssrv_o9.c stubs_o9.c $(DT_REAL)/libauth/auth_proxy.c $(DT_REAL)/libauthsrv/authdial.c $(DT_REAL)/libsec/readcert.c $(LIBS) 2>/dev/null || \
	gcc -O2 $(INCS) -o bin/tlssrv_o9 tlssrv_o9.c stubs_o9.c $(LIBS)

test:
	@echo "Running tests..."
	go test -v ./...

clean:
	@echo "Cleaning..."
	rm -rf bin/
	rm -f tlssrv_o9

install: all
	@echo "Installing binaries to /usr/local/bin..."
	install -m 755 bin/rcpud /usr/local/bin/
	install -m 755 bin/l9term /usr/local/bin/
	install -m 755 bin/drawsrv /usr/local/bin/
	install -m 755 bin/tlssrv_o9 /usr/local/bin/

help:
	@echo "rcpud Build System"
	@echo ""
	@echo "Targets:"
	@echo "  all         - Build all binaries (default)"
	@echo "  rcpud       - Build master namespace server"
	@echo "  aiterm      - Build AI agent shell"
	@echo "  drawsrv     - Build graphics device server"
	@echo "  tlssrv_o9   - Build TLS server helper (C)"
	@echo "  test        - Run tests"
	@echo "  clean       - Remove built binaries"
	@echo "  install     - Install to /usr/local/bin"
	@echo ""
	@echo "Manual builds:"
	@echo "  go build -tags rcpud -o rcpud ."
	@echo "  go build -tags aiterm -o aiterm ."
	@echo "  cd o9draw && go build -tags drawsrv -o drawsrv ."
