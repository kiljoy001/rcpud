#!/bin/bash
export HOME=/home/scott
export NAMESPACE=/tmp/ns.scott.o9
cd /home/scott/Repo/rcpud
exec ./bin/rcpud -l ":17019"
