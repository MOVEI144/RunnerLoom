package core

import "time"

// ImageTransferTimeout is shared by authenticated image streams and the Node
// client. Ordinary JSON requests retain shorter transport deadlines.
const ImageTransferTimeout = 30 * time.Minute
