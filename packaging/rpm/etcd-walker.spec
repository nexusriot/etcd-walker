# Binary-only RPM for etcd-walker. Built by `make rpm-<arch>` (see Makefile),
# which cross-compiles the static Go binary first and passes it in:
#
#   rpmbuild -bb --target <arch> \
#     --define "ew_version 0.11.0" --define "ew_binary /path/to/etcd-walker" \
#     --define "ew_license /path/to/LICENSE" packaging/rpm/etcd-walker.spec
#
# Nothing is compiled here, so the same spec packages any target arch from
# any build host.

# A static, already-stripped Go binary: no debuginfo subpackage, and none of
# the post-install brp scripts (strip, build-id links) — they would run the
# host's arch-specific tools over a foreign-arch binary.
%global debug_package %{nil}
%global __os_install_post %{nil}
%global _build_id_links none

Name:           etcd-walker
Version:        %{ew_version}
Release:        1
Summary:        Terminal UI for browsing and editing etcd (v2 and v3)
License:        MIT
URL:            https://github.com/nexusriot/etcd-walker
Packager:       Vladislav Ananev <nexus.riot@gmail.com>

%description
etcd-walker is a keyboard-driven terminal UI for etcd. It browses the key
space as a directory tree over the v3 or v2 API, edits values with
conflict-safe saves, shows revision history and past revisions, refreshes
live via watches, and offers read-only, dry-run and protected-prefix modes
for working against production clusters.

%install
install -Dm0755 %{ew_binary} %{buildroot}%{_bindir}/etcd-walker
install -Dm0644 %{ew_license} %{buildroot}%{_datadir}/licenses/%{name}/LICENSE

%files
%{_bindir}/etcd-walker
%{_datadir}/licenses/%{name}/LICENSE
