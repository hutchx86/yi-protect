# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 yi-protect contributors
#
# Build yi_protect_snapshot. Reuses the bridge's ring reader and model_table parser;
# FFmpeg (libavcodec/libavutil) and libjpeg-turbo are supplied by build_sd.sh.
CXX ?= g++
CXXFLAGS ?= -O2 -Wall -std=gnu++14
FLV ?= ../flv_bridge
FFMPEG_DIR ?= ../build/snapshot-deps/ffmpeg-8.1.1
JPEG_DIR ?= ../build/snapshot-deps/libjpeg

CPPFLAGS += -I$(FLV) -I$(FFMPEG_DIR) -I$(JPEG_DIR)/include
LDLIBS = $(JPEG_DIR)/lib/libjpeg.a \
         $(FFMPEG_DIR)/libavcodec/libavcodec.a $(FFMPEG_DIR)/libavutil/libavutil.a \
         -lpthread -lm

OBJS = yi_protect_snapshot.o reader.o models.o

.PHONY: all clean

all: yi_protect_snapshot

yi_protect_snapshot: $(OBJS)
	$(CXX) $(CXXFLAGS) -o $@ $(OBJS) $(LDLIBS)

yi_protect_snapshot.o: yi_protect_snapshot.cpp $(FLV)/bridge.h $(FLV)/FshareReader.h
	$(CXX) $(CXXFLAGS) $(CPPFLAGS) -c $< -o $@

reader.o: $(FLV)/FshareReader.cpp $(FLV)/FshareReader.h $(FLV)/bridge.h
	$(CXX) $(CXXFLAGS) $(CPPFLAGS) -c $< -o $@

models.o: $(FLV)/models.cpp $(FLV)/bridge.h
	$(CXX) $(CXXFLAGS) $(CPPFLAGS) -c $< -o $@

clean:
	rm -f $(OBJS) yi_protect_snapshot
