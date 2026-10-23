#!/bin/bash
# Bumps version.txt: feat: -> minor (X.Y+1.0), else patch (X.Y.Z+1).
# Rolls Y/Z over at 10 to match the other rb apps.
version=$(cat version.txt)
IFS='.' read -ra parts <<< "$version"
commit_message=$(git log -1 --pretty=%B)
if [[ $commit_message == feat:* ]]; then
    parts[1]=$((parts[1] + 1)); parts[2]=0
else
    parts[2]=$((parts[2] + 1))
fi
if (( parts[2] == 10 )); then parts[2]=0; parts[1]=$((parts[1] + 1)); fi
if (( parts[1] == 10 )); then parts[1]=0; parts[0]=$((parts[0] + 1)); fi
echo "${parts[0]}.${parts[1]}.${parts[2]}" > version.txt
