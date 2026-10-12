package approve

import "syscall"

// syscallNoCtty keeps opening the approval terminal from making it the
// controlling terminal of the broker.
const syscallNoCtty = syscall.O_NOCTTY
