#!/bin/bash
# shellcheck disable=SC2154
# Explicit experimental X13s profile. No host-only auto-detection/admission.
check() {
    [[ ${DRACUT_ARCH:-$(uname -m)} == aarch64 ]] || return 1
    return 255
}

depends() { echo tbox-live; }

installkernel() {
    # Fedora dracut 108: -c removes the default optional (-o) behavior.
    # --no-kernel must not silently skip this profile's required driver.
    [[ ${no_kernel:-no} != yes ]] || return 1
    # Fedora108 ignores install()'s return but checks installkernel(). Require
    # the success-only marker to propagate failed firmware preparation here.
    grep -qx 'lenovo,thinkpad-x13s' "$initdir/etc/tacklebox/x13s-adsp-required" || return 1
    hostonly='' instmods -c qcom_q6v5_pas || return 1
}

install() {
    # dracut skips the checked kernel hook in --no-kernel mode. This profile
    # cannot produce a meaningful candidate that way; terminate the generator.
    if [[ ${no_kernel:-no} == yes ]]; then
        dfatal 'X13s profile does not support --no-kernel'
        exit 1
    fi
    # Names observed in Fedora kernel-core 7.2.11-200.fc44's X13s DTB.
    # PAS can probe all three enabled remoteprocs when loaded early.
    local name source suffix found config flag
    rm -f "$initdir/etc/tacklebox/x13s-adsp-required" || return 1
    for name in qcadsp8280.mbn qccdsp8280.mbn qcslpi8280.mbn; do
        found=
        for suffix in '' .xz .zst; do
            source="${dracutsysrootdir:-}/usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/${name}${suffix}"
            if [[ -f $source && -s $source ]]; then
                case $suffix in
                    .xz) flag=CONFIG_FW_LOADER_COMPRESS_XZ ;;
                    .zst) flag=CONFIG_FW_LOADER_COMPRESS_ZSTD ;;
                    *) flag= ;;
                esac
                if [[ -n $flag ]]; then
                    config="${srcmods:-${dracutsysrootdir:-}/usr/lib/modules/${kernel:-}}/config"
                    if ! grep -qx "$flag=y" "$config"; then
                        derror "X13s profile cannot prove $flag=y for selected kernel"
                        return 1
                    fi
                fi
                # inst_simple handles source symlinks; final emitted-initrd
                # inspection must verify resolution and content hashes.
                inst_simple "$source" "/usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/${name}${suffix}" || return 1
                found=1
                break
            fi
        done
        if [[ -z $found ]]; then
            derror "X13s profile requires firmware: qcom/sc8280xp/LENOVO/21BX/$name"
            return 1
        fi
    done
    inst_multiple modprobe grep tr readlink || return 1
    # Created only after mandatory inputs have been included. The runtime hook
    # is installed by tbox-live and remains inert for all generic profiles.
    inst_dir /etc/tacklebox || return 1
    printf '%s\n' 'lenovo,thinkpad-x13s' > "$initdir/etc/tacklebox/x13s-adsp-required" || return 1
}
