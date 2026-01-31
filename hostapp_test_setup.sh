#!/bin/bash

set -o errexit
#set -o xtrace

readonly script_name=$(basename "${0}")

export DOCKER_HOST="unix:///var/run/hostapp-docker.sock"

# Real hostapp image for end-to-end testing
HOSTAPP_IMAGE="${HOSTAPP_IMAGE:-bhcr.io/balena_os/raspberrypi4-64/latest}"
# Number of OS block containers to create
OS_BLOCKS_COUNT="${OS_BLOCKS_COUNT:-3}"

usage() {
	cat <<EOF
Usage: ${script_name} [OPTIONS]
	-r Engine root directory
	-n Number of containers with repeated label
	-i Hostapp image to pull (default: ${HOSTAPP_IMAGE})
	-o Number of OS block containers to create (default: ${OS_BLOCKS_COUNT})
	-h Display usage
EOF
}

if which docker > /dev/null 2>&1; then
	ENGINE=docker
elif which balena > /dev/null 2>&1; then
	ENGINE=balena
else
	echo "ERROR: No container engine detected."
	exit 1
fi


setupContainer() {
	label=${1}
	tag=$(uuidgen)
	dockerfile=$(cat <<EOF
FROM busybox:latest
LABEL ${label}=overlay
EOF
)
	echo "${dockerfile}" | "${ENGINE}" build -t "${tag}" - > /dev/null
	cid=$("${ENGINE}" create "${tag}" /bin/fail 2>/dev/null)
	echo "${cid}"
}

# Setup real hostapp container from balena registry
setupRealHostapp() {
	local rootdir=${1}
	echo "Pulling real hostapp: ${HOSTAPP_IMAGE}"
	"${ENGINE}" pull "${HOSTAPP_IMAGE}"
	cid=$("${ENGINE}" create "${HOSTAPP_IMAGE}" /bin/fail 2>/dev/null)
	# Create current symlink pointing to container directory
	ln -sf "${rootdir}/containers/${cid}" "${rootdir}/hostapp-current"
	echo "${cid}"
}

# Setup OS block container with io.balena.image.class=overlay label
# Each OS block has unique files and a fingerprint with md5sums of all regular files.
setupOSBlockContainer() {
	local index=${1:-1}
	tag="osblock-$(uuidgen)"
	dockerfile=$(cat <<'DOCKERFILE'
FROM busybox:latest
LABEL io.balena.image.class=overlay
ARG INDEX=1
RUN echo "OS Block ${INDEX}" > /osblock-marker-${INDEX} && \
    mkdir -p /osblock-${INDEX}-dir && \
    echo "file1 content from osblock ${INDEX}" > /osblock-${INDEX}-dir/file1.txt && \
    echo "file2 content from osblock ${INDEX}" > /osblock-${INDEX}-dir/file2.txt
# Generate fingerprint of all regular files (not symlinks)
RUN find / -path /proc -prune -o -path /sys -prune -o -path /dev -prune -o \
    -type f -print 2>/dev/null | sort | xargs md5sum 2>/dev/null > /.fingerprint-osblock-${INDEX} || true
DOCKERFILE
)
	echo "${dockerfile}" | "${ENGINE}" build --build-arg INDEX="${index}" -t "${tag}" - > /dev/null
	cid=$("${ENGINE}" create "${tag}" /bin/fail 2>/dev/null)
	echo "${cid}"
}

# Setup fingerprinted hostapp container for testing overlay stacking
# This creates a minimal hostapp with known files and a fingerprint of all regular files.
setupFingerprintedHostapp() {
	local rootdir=${1}
	tag="hostapp-fingerprinted-$(uuidgen)"
	dockerfile=$(cat <<'DOCKERFILE'
FROM busybox:latest
# Create some hostapp-specific files
RUN mkdir -p /hostapp-data /etc/hostapp && \
    echo "hostapp config" > /etc/hostapp/config && \
    echo "hostapp data file 1" > /hostapp-data/data1.txt && \
    echo "hostapp data file 2" > /hostapp-data/data2.txt
# Generate fingerprint of all regular files (not symlinks)
RUN find / -path /proc -prune -o -path /sys -prune -o -path /dev -prune -o \
    -type f -print 2>/dev/null | sort | xargs md5sum 2>/dev/null > /.fingerprint-hostapp || true
DOCKERFILE
)
	echo "${dockerfile}" | "${ENGINE}" build -t "${tag}" - > /dev/null
	cid=$("${ENGINE}" create "${tag}" /bin/fail 2>/dev/null)
	# Create fingerprint-current symlink for fingerprinted hostapp testing
	ln -sf "${rootdir}/containers/${cid}" "${rootdir}/fingerprint-current"
	echo "${cid}"
}

pidFile=/var/run/docker-host.pid
runFile=/var/run/docker-host
stopEngine() {
	[ ! -e "${pidFile}" ] && return
	pid=$(cat /var/run/docker-host.pid)
	sudo kill "${pid}"
	sleep 2
	if [ -e "${pidFile}" ]; then
		sudo kill -9 "${pid}"
		sleep 2
	fi
}

startEngine() {
	rootdir=${1}
	overlay=${2}
	mkdir -p /var/run
	if [ -e "${pidFile}" ] || [ -e "${runFile}" ]; then
		stopEngine
	fi
	[ -z "${overlay}" ] && overlay="overlay2"
	sudo ${ENGINE}d -s "${overlay}" --data-root="${rootdir}" -H unix:///var/run/hostapp-docker.sock --pidfile=${pidFile} --exec-root="${runFile}" > /dev/null 2>&1 &
	while [ ! -e "/var/run/hostapp-docker.sock" ]; do
		sleep 2
	done
}

waitEngine() {
	_stime=$(date +%s)
	_etime=$(date +%s)
	_timeout=10
	until DOCKER_HOST=unix:///var/run/hostapp-docker.sock docker info > /dev/null; do
		if [ $(( _etime - _stime )) -le ${_timeout} ]; then
			sleep 1
			_etime=$(date +%s)
		else
			echo "Timeout starting docker"
		fi
	done
}

main() {
	if [ ${#} -eq 0 ] ; then
		usage
		exit 1
	else
		while getopts "hr:n:i:o:" c; do
			case "${c}" in
				r) rootdir="${OPTARG:-}";;
				n) ccount="${OPTARG:-}";;
				i) HOSTAPP_IMAGE="${OPTARG:-}";;
				o) OS_BLOCKS_COUNT="${OPTARG:-}";;
				h) usage;;
				*) usage;exit 1;;
			esac
		done

		[ -z "${rootdir}" ] && echo "Specify an engine root directory" && usage && exit 1
		[ ! -d "${rootdir}" ] && mkdir -p "${rootdir}"

		echo "Starting engine"
		startEngine "${rootdir}"
		waitEngine
		echo "Setting up container images"
		# Create container with unique label
		cid=$(setupContainer "unique-label")
		# Create current symlink to simulate hostapp
		ln -s ${rootdir}/containers/${cid} ${rootdir}/current
		# Create multiple containers with the same label
		[ -z "${ccount}" ] && ccount=10
		for _ in $(seq $ccount); do
			setupContainer "repeated-label"
		done

		# Setup real hostapp from balena registry
		echo "Setting up real hostapp"
		setupRealHostapp "${rootdir}"

		# Setup fingerprinted hostapp for overlay stacking test
		echo "Setting up fingerprinted hostapp"
		setupFingerprintedHostapp "${rootdir}"

		# Setup OS block containers
		echo "Setting up ${OS_BLOCKS_COUNT} OS block containers"
		for i in $(seq ${OS_BLOCKS_COUNT}); do
			setupOSBlockContainer "${i}"
		done

		"${ENGINE}" images
		"${ENGINE}" ps -a
		stopEngine
		sudo chown ${USER}:${USER} ${rootdir} -R || true
	fi
	echo "Done"
}

main "${@}"

