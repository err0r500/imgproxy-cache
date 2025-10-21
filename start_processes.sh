#!/bin/bash

# Start the first process
IMGPROXY_BIND=127.0.0.1:8081 imgproxy &

# Start the second process
proxy &

# Wait for any process to exit
wait -n

# Exit with status of process that exited first
exit $?
