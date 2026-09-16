/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 */

/*
 * cpld_ctl -- control the IR-cut filter and IR LED array via /dev/cpld_periph,
 * bypassing rmm's automatic day/night logic. A second open() while rmm holds
 * the device open succeeds, so no coordination is needed.
 *
 * Commands:
 *   cpld_ctl ircut out      -- filter out (night/IR-passing)
 *   cpld_ctl ircut in       -- filter in (day)
 *   cpld_ctl led <0-100>    -- IR LED level (only 0 and 100 are verified;
 *                              other values may not produce a real ramp)
 */
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <sys/ioctl.h>
#include <unistd.h>

#define CPLD_DEV "/dev/cpld_periph"
#define REQ_IRCUT_OUT 0x7015
#define REQ_IRCUT_IN  0x7016
#define REQ_LED_LEVEL 0x7013

static int open_dev(void) {
    int fd = open(CPLD_DEV, O_RDWR);
    if (fd < 0) {
        perror("open " CPLD_DEV);
        exit(1);
    }
    return fd;
}

static void usage(const char *argv0) {
    fprintf(stderr,
        "usage: %s ircut out|in\n"
        "       %s led <0-100>\n",
        argv0, argv0);
    exit(1);
}

int main(int argc, char **argv) {
    if (argc < 2) usage(argv[0]);

    if (strcmp(argv[1], "ircut") == 0) {
        if (argc != 3) usage(argv[0]);
        long req;
        if (strcmp(argv[2], "out") == 0) req = REQ_IRCUT_OUT;
        else if (strcmp(argv[2], "in") == 0) req = REQ_IRCUT_IN;
        else { usage(argv[0]); return 1; }

        int fd = open_dev();
        if (ioctl(fd, req, 0) < 0) {
            perror("ioctl ircut");
            close(fd);
            return 1;
        }
        close(fd);
        return 0;
    }

    if (strcmp(argv[1], "led") == 0) {
        if (argc != 3) usage(argv[0]);
        int32_t level = (int32_t)strtol(argv[2], NULL, 0);
        if (level < 0) level = 0;
        if (level > 100) level = 100;

        int fd = open_dev();
        if (ioctl(fd, REQ_LED_LEVEL, &level) < 0) {
            perror("ioctl led");
            close(fd);
            return 1;
        }
        close(fd);
        return 0;
    }

    usage(argv[0]);
    return 1;
}
