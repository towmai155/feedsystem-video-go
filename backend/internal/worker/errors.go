package worker

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

// isDuplicateKey reports MySQL error 1062, raised by a unique index.
func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
