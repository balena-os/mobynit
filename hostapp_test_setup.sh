#!/bin/bash

set -o errexit
#set -o xtrace

readonly script_name=$(basename "${0}")

export DOCKER_HOST="unix:///var/run/hostapp-docker.sock"

usage() {
	cat <<EOF
Usage: ${script_name} [OPTIONS]
	-r Engine root directory
	-n Number of containers with repeated label
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
LABEL ${label}=1
EOF
)
	echo "${dockerfile}" | "${ENGINE}" build -t "${tag}" - > /dev/null
	cid=$("${ENGINE}" create "${tag}" /bin/fail 2>/dev/null)
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
		while getopts "hr:n:" c; do
			case "${c}" in
				r) rootdir="${OPTARG:-}";;
				n) ccount="${OPTARG:-}";;
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
		"${ENGINE}" images
		"${ENGINE}" ps -a
		stopEngine
		sudo chown ${USER}:${USER} ${rootdir} -R || true
	fi
	echo "Done"
}

main "${@}"

