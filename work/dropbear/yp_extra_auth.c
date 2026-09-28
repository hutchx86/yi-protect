/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Second password per account for dropbear, #included into svr-authpasswd.c
 * by svr-authpasswd.patch (after constant_time_strcmp).
 *
 * /etc/shadow holds the unifi.cfg SSH_PASSWORD hash; YP_EXTRA_HASH_FILE holds
 * "user:hash" lines: the controller's UpdateUsernamePassword push (a SHA-512
 * "$6$" crypt of the Protect device password), which ssh-accounts.sh copies to
 * the tmpfs /etc only while unifi.cfg PROTECT_SSH is on. Either credential
 * logs the user in. "$6$" is verified with the bundled yp_crypt_sha512 (the
 * camera libc cannot); anything else goes through libc crypt().
 */
/* libtomcrypt (via includes.h) already defines sha512_init and friends. */
#define sha512_init   yp_sha512_init
#define sha512_update yp_sha512_update
#define sha512_sum    yp_sha512_sum
#define processblock  yp_sha512_processblock
#define pad           yp_sha512_pad
#define hashmd        yp_sha512_hashmd
#define to64          yp_sha512_to64
#define sha512crypt   yp_sha512crypt
#include "yp_crypt_sha512.c"
#undef sha512_init
#undef sha512_update
#undef sha512_sum
#undef processblock
#undef pad
#undef hashmd
#undef to64
#undef sha512crypt

#ifndef YP_EXTRA_HASH_FILE
#define YP_EXTRA_HASH_FILE "/etc/ssh_protect"
#endif

/* 1 if password matches a YP_EXTRA_HASH_FILE entry for user. Calls libc
 * crypt() for non-$6$ entries, so run it before the caller's own crypt(),
 * whose static result buffer it would overwrite. */
static int yp_extra_password_ok(const char *user, const char *password) {
	char line[512];
	char out[128];
	size_t ul = strlen(user);
	int ok = 0;
	FILE *f = fopen(YP_EXTRA_HASH_FILE, "r");

	if (f == NULL) {
		return 0;
	}
	while (!ok && fgets(line, sizeof(line), f) != NULL) {
		const char *hash, *res;

		line[strcspn(line, "\r\n")] = '\0';
		if (strncmp(line, user, ul) != 0 || line[ul] != ':') {
			continue;
		}
		hash = line + ul + 1;
		if (hash[0] == '\0' || hash[0] == '!' || hash[0] == '*') {
			continue;
		}
		if (strncmp(hash, "$6$", 3) == 0) {
			res = yp_crypt_sha512(password, hash, out);
		} else {
			res = crypt(password, hash);
		}
		if (res != NULL && res[0] != '*' && constant_time_strcmp(res, hash) == 0) {
			ok = 1;
		}
	}
	fclose(f);
	m_burn(line, sizeof(line));
	m_burn(out, sizeof(out));
	return ok;
}
