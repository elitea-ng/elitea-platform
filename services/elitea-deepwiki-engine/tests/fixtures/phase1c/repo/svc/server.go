package svc

import "github.com/gin-gonic/gin"

// User is the wire shape of a user.
type User struct {
	ID       int    `json:"id"`
	FullName string `json:"full_name,omitempty"`
	IsActive bool   `json:"is_active"`
}

// CartServiceServer is the gRPC server interface.
type CartServiceServer interface {
	GetCart(ctx Context, req *GetCartRequest) (*Cart, error)
	EmptyCart(ctx Context, req *EmptyCartRequest) (*Empty, error)
}

// Routes registers the HTTP routes.
func Routes(r *gin.Engine) {
	r.GET("/api/v1/users/me", readMe)
	r.POST("/users/:id/hash", hashUser)
}

func readMe(c *gin.Context)   {}
func hashUser(c *gin.Context) {}
