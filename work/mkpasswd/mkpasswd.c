/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * mkpasswd -- print an MD5-crypt ($1$) hash of a plaintext password.
 *
 * The camera's musl 1.1.16 libc implements only DES and MD5 crypt, and the
 * dropbear sshd verifies /etc/shadow through that same crypt(), so $1$ is the
 * one scheme guaranteed to round-trip there. unifi/script/init.sh pipes
 * unifi.cfg's SSH_PASSWORD through this and installs the result as root's
 * shadow entry (the stock image ships an empty root password plus dropbear's
 * -B, i.e. a wide-open login).
 *
 * Usage:
 *   mkpasswd <password>
 *   printf '%s\n' <password> | mkpasswd
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <time.h>
#include <fcntl.h>
#include <crypt.h>

/* crypt()'s salt alphabet. */
static const char salt_alphabet[] =
    "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

int main(int argc, char **argv)
{
    char password[256];
    unsigned char rnd[8];
    char salt[9];
    char setting[16];
    const char *hash;
    int fd = -1, i;

    if (argc > 1) {
        snprintf(password, sizeof password, "%s", argv[1]);
    } else {
        if (!fgets(password, sizeof password, stdin))
            return 1;
        password[strcspn(password, "\r\n")] = '\0';
    }

    fd = open("/dev/urandom", O_RDONLY);
    if (fd < 0 || read(fd, rnd, sizeof rnd) != (ssize_t)sizeof rnd) {
        unsigned long seed = (unsigned long)time(NULL) ^
                             ((unsigned long)getpid() << 16);
        for (i = 0; i < (int)sizeof rnd; i++) {
            seed = seed * 1103515245UL + 12345UL;
            rnd[i] = (unsigned char)(seed >> 16);
        }
    }
    if (fd >= 0)
        close(fd);

    for (i = 0; i < 8; i++)
        salt[i] = salt_alphabet[rnd[i] % (sizeof salt_alphabet - 1)];
    salt[8] = '\0';

    snprintf(setting, sizeof setting, "$1$%s$", salt);
    hash = crypt(password, setting);
    if (!hash) {
        fprintf(stderr, "mkpasswd: crypt() failed\n");
        return 1;
    }

    printf("%s\n", hash);
    return 0;
}
