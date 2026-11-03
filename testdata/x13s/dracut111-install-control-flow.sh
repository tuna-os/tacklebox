# shellcheck shell=bash
# The sourcing dracut main loop supplies these four variables.
# shellcheck disable=SC2154
# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright 2005-2013 Red Hat, Inc. All rights reserved.
# License text: dracut111-install-control-flow.sh.LICENSE
# Exact control flow from Fedora dracut111-4.fc45 /usr/bin/dracut lines2720-2733.
# RPM SHA256 186687a21646d13802203e529500c2c477032fb564e5afcfe4f41bdff3a82eaa
# (byte-identical dispatch to the dracut108-8.fc44 excerpt it replaces).
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
