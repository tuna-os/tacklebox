# shellcheck shell=bash
# The sourcing dracut main loop supplies these four variables.
# shellcheck disable=SC2154
# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright 2005-2013 Red Hat, Inc. All rights reserved.
# License text: dracut108-install-control-flow.sh.LICENSE
# Exact control flow from Fedora dracut108-8.fc44 /usr/bin/dracut lines2075-2089.
# RPM SHA256 d503e00739a79e6b326804183da66a7e62ae3cd5663b93777e2b75374bd6219b
# Only module callbacks are supplied by the test; no installed dracut is run.
    if [[ $kernel_only == yes ]]; then
        module_installkernel "$_d_mod" "$moddir" || {
            dfatal "installkernel failed in module $_d_mod"
            exit 1
        }
    else
        module_install "$_d_mod" "$moddir"
        if [[ $no_kernel != yes ]]; then
            module_installkernel "$_d_mod" "$moddir" || {
                dfatal "installkernel failed in module $_d_mod"
                exit 1
            }
        fi
    fi
