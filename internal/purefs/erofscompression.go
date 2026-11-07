package purefs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tuna-os/tacklebox/internal/oci"
)

const (
	ErofsCompressionNone = "none"
	ErofsCompressionLZ4  = "lz4"

	maxErofsKernelConfigBytes = 1 << 20
)

// NormalizeRootFSCompression resolves the optional selector without guessing a
// codec. Callers retain the original empty selector to distinguish omission
// from explicit "none" when resolving conflicting input sources.
func NormalizeRootFSCompression(mode string) (string, error) {
	switch mode {
	case "", ErofsCompressionNone:
		return ErofsCompressionNone, nil
	case ErofsCompressionLZ4:
		return ErofsCompressionLZ4, nil
	default:
		return "", fmt.Errorf("unsupported rootfsCompression %q (valid: none, lz4)", mode)
	}
}

var erofsKernelVersionPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+))?(?:[-+][A-Za-z0-9][A-Za-z0-9._+-]*)?$`)

// erofsKernelVersion takes only a canonical numeric upstream prefix from the
// selected release; distribution suffixes never confer filesystem capability.
func erofsKernelVersion(release string) ([3]int, error) {
	var version [3]int
	parts := erofsKernelVersionPattern.FindStringSubmatch(release)
	if parts == nil {
		return version, fmt.Errorf("unknown kernel version %q", release)
	}
	for i := range version {
		part := parts[i+1]
		if part == "" {
			continue
		}
		if len(part) > 1 && part[0] == '0' {
			return version, fmt.Errorf("noncanonical kernel version %q", release)
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return version, fmt.Errorf("unknown kernel version %q: %w", release, err)
		}
		version[i] = n
	}
	if version[0] == 0 {
		return version, fmt.Errorf("unknown kernel version %q", release)
	}
	return version, nil
}

// ValidateErofsCompression admits LZ4 only from the exact selected kernel's
// source-tree config and upstream big-pcluster version support. It does not
// grant runtime qualification or consult image-family/user-supplied capability
// claims. Uncompressed output needs neither a kernel version nor a config read.
func ValidateErofsCompression(root *oci.Node, store oci.BlobStore, kernelVersion, mode string) error {
	normalized, err := NormalizeRootFSCompression(mode)
	if err != nil {
		return err
	}
	if normalized == ErofsCompressionNone {
		return nil
	}
	version, err := erofsKernelVersion(kernelVersion)
	if err != nil {
		return err
	}
	if version[0] < 5 || (version[0] == 5 && version[1] < 13) {
		return fmt.Errorf("kernel %q lacks EROFS big-pcluster support (requires >= 5.13)", kernelVersion)
	}
	if root == nil || store == nil {
		return fmt.Errorf("kernel %q config unavailable for lz4", kernelVersion)
	}
	configPath := "usr/lib/modules/" + kernelVersion + "/config"
	if root.Lookup(configPath) == nil {
		configPath = "boot/config-" + kernelVersion
	}
	_, node := resolveTreeFile(root, configPath)
	if node == nil {
		return fmt.Errorf("kernel %q config unavailable: %s is not a regular file", kernelVersion, configPath)
	}
	if node.Size < 0 || node.Size > maxErofsKernelConfigBytes {
		return fmt.Errorf("kernel config %s size %d outside 0..%d bytes", configPath, node.Size, maxErofsKernelConfigBytes)
	}
	r, err := store.Open(node.Ref)
	if err != nil {
		return fmt.Errorf("open kernel config %s: %w", configPath, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(r, maxErofsKernelConfigBytes+1))
	closeErr := r.Close()
	if readErr != nil {
		return fmt.Errorf("read kernel config %s: %w", configPath, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close kernel config %s: %w", configPath, closeErr)
	}
	if len(body) > maxErofsKernelConfigBytes {
		return fmt.Errorf("kernel config %s exceeds %d bytes", configPath, maxErofsKernelConfigBytes)
	}
	if int64(len(body)) != node.Size {
		return fmt.Errorf("kernel config %s size drift: read %d bytes, expected %d", configPath, len(body), node.Size)
	}
	if err := validateErofsKernelConfig(body, version); err != nil {
		return fmt.Errorf("kernel %q config %s: %w", kernelVersion, configPath, err)
	}
	return nil
}

func validateErofsKernelConfig(body []byte, version [3]int) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("kernel config is not valid UTF-8")
	}
	settings := [...]struct {
		key   string
		value string
		seen  bool
	}{
		{key: "CONFIG_EROFS_FS"},
		{key: "CONFIG_EROFS_FS_ZIP"},
		{key: "CONFIG_LZ4_DECOMPRESS"},
	}
	headerSeen := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(nil, maxErofsKernelConfigBytes+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "# Linux/") {
			if headerSeen {
				return fmt.Errorf("duplicate kernel configuration version header")
			}
			headerSeen = true
			fields := strings.Fields(line)
			if len(fields) != 5 || fields[3] != "Kernel" || fields[4] != "Configuration" {
				return fmt.Errorf("malformed kernel configuration version header")
			}
			configVersion, err := erofsKernelVersion(fields[2])
			if err != nil {
				return fmt.Errorf("kernel configuration version: %w", err)
			}
			if configVersion != version {
				return fmt.Errorf("kernel configuration version %q does not match selected kernel", fields[2])
			}
		}
		for i := range settings {
			s := &settings[i]
			value := ""
			switch {
			case strings.HasPrefix(line, s.key+"="):
				value = strings.TrimPrefix(line, s.key+"=")
				if value != "y" && value != "m" && value != "n" {
					return fmt.Errorf("malformed %s setting %q", s.key, line)
				}
			case line == "# "+s.key+" is not set":
				value = "n"
			case line == s.key || strings.HasPrefix(line, s.key+" ") || strings.HasPrefix(line, s.key+"\t"),
				line == "# "+s.key || strings.HasPrefix(line, "# "+s.key+" ") || strings.HasPrefix(line, "# "+s.key+"="):
				return fmt.Errorf("malformed %s setting %q", s.key, line)
			default:
				continue
			}
			if s.seen {
				return fmt.Errorf("duplicate or contradictory %s setting", s.key)
			}
			s.seen, s.value = true, value
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("parse kernel config: %w", err)
	}
	for i, s := range settings {
		if !s.seen {
			return fmt.Errorf("missing %s", s.key)
		}
		if s.value != "y" && !(i == 0 && s.value == "m") {
			required := "y"
			if i == 0 {
				required = "y or m"
			}
			return fmt.Errorf("lz4 requires %s=%s (found %s)", s.key, required, s.value)
		}
	}
	return nil
}
