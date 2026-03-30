package auth

import "testing"

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("mysecretpassword")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if hash == "" {
		t.Error("hash is empty")
	}
	if hash == "mysecretpassword" {
		t.Error("hash should not equal the plaintext password")
	}
}

func TestHashPassword_DifferentHashes(t *testing.T) {
	hash1, err := HashPassword("password")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	hash2, err := HashPassword("password")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	// bcrypt uses random salt, so two hashes of the same password differ
	if hash1 == hash2 {
		t.Error("two hashes of the same password should differ due to random salt")
	}
}

func TestCheckPassword_Correct(t *testing.T) {
	hash, err := HashPassword("correctpassword")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if !CheckPassword(hash, "correctpassword") {
		t.Error("CheckPassword should return true for correct password")
	}
}

func TestCheckPassword_Wrong(t *testing.T) {
	hash, err := HashPassword("correctpassword")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if CheckPassword(hash, "wrongpassword") {
		t.Error("CheckPassword should return false for wrong password")
	}
}

func TestCheckPassword_EmptyPassword(t *testing.T) {
	hash, err := HashPassword("notempty")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if CheckPassword(hash, "") {
		t.Error("CheckPassword should return false for empty password against non-empty hash")
	}
}
