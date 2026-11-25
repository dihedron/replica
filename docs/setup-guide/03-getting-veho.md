# Getting veho

## From a release

Releases are published on GitHub, at <https://github.com/dihedron/veho/releases>, with `.deb`, `.rpm` and `.apk` packages, archives and SBOMs, and these documents. No release has been published yet; when one is, verify it before you install it.

Every release is signed with the project's packaging key, `packaging/signing-key.asc` in the repository, fingerprint `FB7DE97BAAFAA2A676CEE59A1B34E6A2A14CD06A`. Take the key from the repository, not from the release you are verifying.

```bash
gpg --import signing-key.asc
gpg --verify checksums.txt.asc checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

The `.rpm` packages carry their own signature (`rpm --checksig`, after `rpm --import signing-key.asc`); the `.deb` packages carry a `debsig` origin signature, which `debsig-verify` checks with a policy for the key (the project's README gives the commands).

Install the package on every node, with the package of each storage driver the node needs other than `file` and `http`: `veho-plugin-s3` for S3, `veho-plugin-sftp` for SFTP and SCP, `veho-plugin-ftp` for FTP, `veho-plugin-glance` and `veho-plugin-swift` for OpenStack's image and object services, `veho-plugin-nexus` for Sonatype Nexus Repository 3. A plugin package installs its binary under `/usr/libexec/veho/plugins/`, with a manifest signed with the release's key, which the `veho` package installs as `/etc/veho/plugin-keys/veho.pub`; the release's plugins run only with veho of the same release, so install and upgrade them together.

```bash
sudo apt install ./veho_<version>_linux_amd64.deb ./veho-plugin-s3_<version>_linux_amd64.deb      # Ubuntu
sudo dnf install ./veho_<version>_linux_amd64.rpm ./veho-plugin-s3_<version>_linux_amd64.rpm      # Rocky Linux, AlmaLinux
```

## From the source

Until the first release, build from a clone of the repository, with Go (the version in `go.mod`) and GoReleaser:

```bash
git clone https://github.com/dihedron/veho.git
cd veho
make compile        # dist/veho_linux_amd64_v1/veho
```

Then copy the binary to `/usr/bin/veho` on every node, and install `packaging/veho.service` as `/usr/lib/systemd/system/veho.service`; run `packaging/postinstall.sh` once on each node to create the user and `/etc/veho`.

Check the binary:

```bash
veho version            # one line
veho version --verbose  # version, build, licence
```
