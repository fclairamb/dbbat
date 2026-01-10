# Security Measures

## Passwords & Keys

### Hashing
All DBBat passwords and keys are hashed using argon2id, a strong password hashing algorithm that is designed to 
be resistant to brute-force attacks.

## Cyphering
DBBat uses AES-256-GCM for encrypting sensitive data such as database credentials.
The encryption keys are stored and never transmitted over the network.

## APIs

### Operations
All operations must be done through API keys (`dbb_*`) or Web tokens (`web_*`).

Login and passwords are only used for connecting and creating keys.

### Restricted listing
A connector/user cannot list other users, databases parameters or queries. It can only list the connections.

## PostgreSQL connection

### Limited access
The granted access can be limited:
- To a specific time window
- To a maximum number of queries
- To a maximum amount of data exchanged

It's recommended to use all of these restrictions to make sure a connection is not abused.

### Read mode
The read-mode is a "best-effort" approach to ensure that we're not performing any write operation on the database.
There are few restrictions in place to prevent accidental writes.
It shall NOT be used to secure read access to potential bad actors (like external actors). To do that you should
create a dedicated user with read-only privileges.

Here are the list of mechanisms used to secure the read-mode:

#### Command protection
We're preventing the execution of:
- DML statements (`INSERT`, `UPDATE`, `DELETE`)
- DDL statements (`CREATE`, `ALTER`, `DROP`)
- DCL statements (`GRANT`, `REVOKE`)

#### Parameter protection
In the read-mode, we're starting the connection with `default_transaction_read_only=true` and 
we're preventing to set the `default_transaction_read_only=off` parameter.
