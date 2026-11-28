#!/bin/sh
# metis-agent.sh ROLE SLICE SLUG STEPS stands in for a coding agent that
# Metis starts through `gaia run --wait`. It runs in the node's checkout and
# uses only metis and git, as a real agent would.
#
# First it runs each line of the file STEPS, if it exists, in order:
#
#   print:TEXT   write TEXT and a newline
#   sleep:SECS   sleep
#   exit:N       exit with code N
#
# Then its role, as agent SLUG on SLICE:
#
#   coder     commit a file through `metis commit`, verify, flip coded
#   reviewer  flip reviewed
set -eu

role=$1
slice=$2
slug=$3
steps=$4

echo "agent: $role $slice in $(pwd -P)"
if [ -f "$steps" ]; then
	while IFS= read -r step; do
		case $step in
		print:*) echo "${step#print:}" ;;
		sleep:*) sleep "${step#sleep:}" ;;
		exit:*) exit "${step#exit:}" ;;
		'') ;;
		*)
			echo "agent: unknown step $step" >&2
			exit 64
			;;
		esac
	done < "$steps"
fi

case $role in
coder)
	echo "work by $slug" > "work-$slice.txt"
	git add "work-$slice.txt"
	metis commit -m "agent work" --slice "$slice"
	metis verify --post --slice "$slice"
	metis commit --flip coded --slice "$slice"
	;;
reviewer)
	metis commit --flip reviewed --slice "$slice" --agent "$slug"
	;;
*)
	echo "agent: unknown role $role" >&2
	exit 64
	;;
esac
echo "agent: $role done"
