#!/bin/bash

UNAME=$(uname -a)
COMMIT_HASH="$(git rev-parse --short HEAD)"

if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root. sudo ./start.sh"
    exit 1
fi

# Preserve PATH to use the correct Go installation (e.g., from gvm)
if [[ -n "$SUDO_USER" ]]; then
    USER_HOME=$(eval echo ~$SUDO_USER)
    # Try to source gvm if it exists and use Go 1.23.12
    if [[ -f "$USER_HOME/.gvm/scripts/gvm" ]]; then
        source "$USER_HOME/.gvm/scripts/gvm"
        # Explicitly use Go 1.23.12 if available
        if gvm use go1.23.12 &>/dev/null; then
            echo "Switched to Go 1.23.12 via gvm"
        fi
    fi
    # Also add common gvm paths - prioritize Go 1.23.12
    if [[ -d "$USER_HOME/.gvm/gos" ]]; then
        # Try to find Go 1.23.12 first, then fall back to newest
        if [[ -x "$USER_HOME/.gvm/gos/go1.23.12/bin/go" ]]; then
            export PATH="$USER_HOME/.gvm/gos/go1.23.12/bin:$PATH"
        else
            # Find the newest Go version
            for gvm_go in $(ls -d "$USER_HOME/.gvm/gos"/go*/bin/go 2>/dev/null | sort -V -r); do
                if [[ -x "$gvm_go" ]]; then
                    export PATH="$(dirname "$gvm_go"):$PATH"
                    break
                fi
            done
        fi
    fi
fi

# Find Go binary - prefer the one from PATH
GO_CMD=$(command -v go 2>/dev/null || echo "go")

# Verify Go version and set GOMODCACHE correctly
if command -v go &>/dev/null; then
    GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    echo "Using Go version: $GO_VERSION"
    
    # Ensure GOMODCACHE points to the correct Go version's cache
    # Unset it first to override any OS environment variable
    unset GOMODCACHE
    
    # If using gvm, set GOMODCACHE to match the current Go version
    if [[ -n "$SUDO_USER" ]]; then
        USER_HOME=$(eval echo ~$SUDO_USER)
        if [[ -d "$USER_HOME/.gvm/pkgsets" ]]; then
            # Extract Go version (e.g., "1.23.12" from "go1.23.12")
            GO_VER=$(echo "$GO_VERSION" | sed 's/go//')
            GVM_CACHE="$USER_HOME/.gvm/pkgsets/go${GO_VER}/global/pkg/mod"
            if [[ -d "$(dirname "$GVM_CACHE")" ]]; then
                export GOMODCACHE="$GVM_CACHE"
            fi
        fi
    fi
fi

if [[ -d ./chipper ]]; then
    cd chipper
fi

#if [[ ! -f ./chipper ]]; then
#   if [[ -f ./go.mod ]]; then
#     echo "You need to build chipper first. This can be done with the setup.sh script."
#   else
#     echo "You must be in the chipper directory."
#   fi
#   exit 0
#fi

if [[ ! -f ./source.sh ]]; then
    echo "You need to make a source.sh file. This can be done with the setup.sh script."
    exit 0
fi

source source.sh

# set go tags
export GOTAGS="nolibopusfile"

if [[ ${USE_INBUILT_BLE} == "true" ]]; then
    GOTAGS="${GOTAGS},inbuiltble"
fi

export GOLDFLAGS="-X 'github.com/kercre123/wire-pod/chipper/pkg/vars.CommitSHA=${COMMIT_HASH}'"

# Ensure go.mod is up to date before running
if command -v go &>/dev/null && [[ -f ./go.mod ]]; then
    echo "Ensuring go.mod is up to date..."
    $GO_CMD mod tidy 2>&1 | grep -v "go: downloading" || true
fi

#./chipper
if [[ ${STT_SERVICE} == "leopard" ]]; then
    if [[ -f ./chipper ]]; then
        ./chipper
    else
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/leopard/main.go
    fi
    elif [[ ${STT_SERVICE} == "rhino" ]]; then
    if [[ -f ./chipper ]]; then
        ./chipper
    else
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/experimental/rhino/main.go
    fi
    elif [[ ${STT_SERVICE} == "houndify" ]]; then
    if [[ -f ./chipper ]]; then
        ./chipper
    else
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/experimental/houndify/main.go
    fi
    elif [[ ${STT_SERVICE} == "xiaozhi" ]]; then
    if [[ -f ./chipper ]]; then
        ./chipper
    else
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/experimental/xiaozhi/main.go
    fi
    elif [[ ${STT_SERVICE} == "whisper" ]]; then
    if [[ -f ./chipper ]]; then
        ./chipper
    else
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/experimental/whisper/main.go
    fi
    elif [[ ${STT_SERVICE} == "whisper.cpp" ]]; then
    if [[ -f ./chipper ]]; then
        export C_INCLUDE_PATH="../whisper.cpp"
        export LIBRARY_PATH="../whisper.cpp"
        export LD_LIBRARY_PATH="$LD_LIBRARY_PATH:$(pwd)/../whisper.cpp:$(pwd)/../whisper.cpp/build:$(pwd)/../whisper.cpp/build/src"
        export CGO_LDFLAGS="-L$(pwd)/../whisper.cpp/build_go/src"
        export CGO_CFLAGS="-I$(pwd)/../whisper.cpp"
        ./chipper
    else
        export C_INCLUDE_PATH="../whisper.cpp"
        export LIBRARY_PATH="../whisper.cpp"
        export LD_LIBRARY_PATH="$LD_LIBRARY_PATH:$(pwd)/../whisper.cpp:$(pwd)/../whisper.cpp/build:$(pwd)/../whisper.cpp/build_go/src:$(pwd)/../whisper.cpp/build_go/ggml/src"
        export CGO_LDFLAGS="-L$(pwd)/../whisper.cpp -L$(pwd)/../whisper.cpp/build -L$(pwd)/../whisper.cpp/build/src -L$(pwd)/../whisper.cpp/build_go/ggml/src -L$(pwd)/../whisper.cpp/build_go/src"
        export CGO_CFLAGS="-I$(pwd)/../whisper.cpp -I$(pwd)/../whisper.cpp/include -I$(pwd)/../whisper.cpp/ggml/include"
        if [[ ${UNAME} == *"Darwin"* ]]; then
            export GGML_METAL_PATH_RESOURCES="../whisper.cpp"
            $GO_CMD run -tags $GOTAGS -ldflags "-extldflags '-framework Foundation -framework Metal -framework MetalKit'" cmd/experimental/whisper.cpp/main.go
        else
            $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/experimental/whisper.cpp/main.go
        fi
    fi
    elif [[ ${STT_SERVICE} == "vosk" ]]; then
    if [[ -f ./chipper ]]; then
        export CGO_ENABLED=1
        export CGO_CFLAGS="-I/root/.vosk/libvosk"
        export CGO_LDFLAGS="-L /root/.vosk/libvosk -lvosk -ldl -lpthread"
        export LD_LIBRARY_PATH="/root/.vosk/libvosk:$LD_LIBRARY_PATH"
        ./chipper
    else
        export CGO_ENABLED=1
        export CGO_CFLAGS="-I$HOME/.vosk/libvosk -I/root/.vosk/libvosk"
        export CGO_LDFLAGS="-L$HOME/.vosk/libvosk -L/root/.vosk/libvosk -lvosk -ldl -lpthread"
        export LD_LIBRARY_PATH="/root/.vosk/libvosk:$HOME/.vosk/libvosk:$LD_LIBRARY_PATH"
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" -exec "env DYLD_LIBRARY_PATH=$HOME/.vosk/libvosk" cmd/vosk/main.go
    fi
else
    if [[ -f ./chipper ]]; then
        export CGO_LDFLAGS="-L/root/.coqui/"
        export CGO_CXXFLAGS="-I/root/.coqui/"
        export LD_LIBRARY_PATH="/root/.coqui/:$LD_LIBRARY_PATH"
        ./chipper
    else
        export CGO_LDFLAGS="-L$HOME/.coqui/"
        export CGO_CXXFLAGS="-I$HOME/.coqui/"
        export LD_LIBRARY_PATH="$HOME/.coqui/:$LD_LIBRARY_PATH"
        $GO_CMD run -tags $GOTAGS -ldflags="${GOLDFLAGS}" cmd/coqui/main.go
    fi
fi
