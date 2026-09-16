# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build unifi_flv_bridge. The cross-compiler is supplied by work/build_sd.sh
# (CXX=arm-openwrt-linux-g++); a native build works too for logic testing.
CXX ?= g++
CXXFLAGS ?= -O2 -Wall -std=gnu++14
LDLIBS ?= -lpthread

# FAAD2 AAC decoder + libopus encoder, used by main.cpp's AAC->Opus transcode
# for the FLV type-10 audio track. Paths are supplied by work/build_sd.sh:
# FAAD2=<faad2 build dir with include/ + libfaad.a>, OPUS=<libopus source dir>.
# Left empty for a host build (main.cpp resolves the symbols at link time, so a
# real build needs both; the host `test` target links neither, and it only
# exercises reader code).
FAAD2 ?=
OPUS  ?=
EXTRA_INC = $(if $(FAAD2),-I$(FAAD2)/include) $(if $(OPUS),-I$(OPUS)/include)
EXTRA_LIB = $(if $(FAAD2),$(FAAD2)/libfaad.a) $(if $(OPUS),$(OPUS)/.libs/libopus.a)

# `override` so these survive a CXXFLAGS=... on the make command line
# (work/build_sd.sh passes one).
override CXXFLAGS += $(EXTRA_INC)
override LDLIBS += $(EXTRA_LIB) -lm

SRCS = main.cpp FlvPush.cpp FshareReader.cpp models.cpp
OBJS = $(SRCS:.cpp=.o)

.PHONY: all clean test

all: unifi_flv_bridge

unifi_flv_bridge: $(OBJS)
	$(CXX) $(CXXFLAGS) -o $@ $(OBJS) $(LDLIBS)

%.o: %.cpp bridge.h FlvPush.h FshareReader.h
	$(CXX) $(CXXFLAGS) -c $< -o $@

# Host-only parser test (synthetic shm ring); always builds/runs native.
test: test_fshare_reader.cpp FshareReader.cpp models.cpp
	$(CXX) -O1 -Wall -std=gnu++14 -I. -o test_fshare_reader \
	    test_fshare_reader.cpp FshareReader.cpp models.cpp -lpthread
	./test_fshare_reader
	rm -f test_fshare_reader

clean:
	rm -f $(OBJS) unifi_flv_bridge test_fshare_reader
