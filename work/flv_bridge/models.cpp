/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (C) 2026 yi-protect contributors
 *
 * Per-camera fshare ring geometry. The offset is where the first frame
 * starts inside the ring; the header size is how many bytes of vendor
 * metadata precede each frame payload. Both were reverse engineered per
 * camera family.
 */
#include "bridge.h"

#include <strings.h>

ModelParams modelParams(int model) {
    switch (model) {
    case Y20GA:
    case Y25GA:
    case Y30QA:
        return {300, 22};
    case Y501GC:
        return {368, 24};
    case Y21GA:
    case Y211GA:
    case Y211BA:
    case Y213GA:
    case Y291GA:
    case H30GA:
    case H51GA:
    case H52GA:
    case H60GA:
    case Y28GA:
    case Y29GA:
    case Y623:
        return {368, 28};
    case R40GA:
    case Q321BR_LSX:
    case QG311R:
    case B091QP:
        return {300, 26};
    case R30GB:
    case R35GB:
    case R37GB:
        // Geometry not known a priori on this family; the reader probes it.
        return {0, 0};
    default:
        return {368, 28};
    }
}

int parseModel(const char *name) {
    if (name == nullptr) return Y21GA;
    if (strcasecmp(name, "y20ga") == 0) return Y20GA;
    if (strcasecmp(name, "y25ga") == 0) return Y25GA;
    if (strcasecmp(name, "y30qa") == 0) return Y30QA;
    if (strcasecmp(name, "y501gc") == 0) return Y501GC;
    if (strcasecmp(name, "y21ga") == 0) return Y21GA;
    if (strcasecmp(name, "y211ga") == 0) return Y211GA;
    if (strcasecmp(name, "y211ba") == 0) return Y211BA;
    if (strcasecmp(name, "y213ga") == 0) return Y213GA;
    if (strcasecmp(name, "y291ga") == 0) return Y291GA;
    if (strcasecmp(name, "h30ga") == 0) return H30GA;
    if (strcasecmp(name, "r30gb") == 0) return R30GB;
    if (strcasecmp(name, "r35gb") == 0) return R35GB;
    if (strcasecmp(name, "r37gb") == 0) return R37GB;
    if (strcasecmp(name, "r40ga") == 0) return R40GA;
    if (strcasecmp(name, "h51ga") == 0) return H51GA;
    if (strcasecmp(name, "h52ga") == 0) return H52GA;
    if (strcasecmp(name, "h60ga") == 0) return H60GA;
    if (strcasecmp(name, "y28ga") == 0) return Y28GA;
    if (strcasecmp(name, "y29ga") == 0) return Y29GA;
    if (strcasecmp(name, "y623") == 0) return Y623;
    if (strcasecmp(name, "q321br_lsx") == 0) return Q321BR_LSX;
    if (strcasecmp(name, "qg311r") == 0) return QG311R;
    if (strcasecmp(name, "b091qp") == 0) return B091QP;
    return Y21GA;
}
