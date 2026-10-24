package connections

import (
	"errors"
	"testing"
)

func validTestInput() Input {
	return Input{
		Name:         "Test Database",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "test_db",
		Username:     "test_user",
		Password:     "StrongPassword123!",
		SSLMode:      "require",
	}
}

func TestValidateValidInput(t *testing.T) {
	input := validTestInput()

	if err := validate(input, true); err != nil {
		t.Fatalf("validate() error = %v, want nil", err)
	}
}

func TestValidateInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{
			name: "empty name",
			mutate: func(i *Input) {
				i.Name = ""
			},
		},
		{
			name: "name with surrounding spaces",
			mutate: func(i *Input) {
				i.Name = " Test Database "
			},
		},
		{
			name: "empty host",
			mutate: func(i *Input) {
				i.Host = ""
			},
		},
		{
			name: "port too small",
			mutate: func(i *Input) {
				i.Port = 0
			},
		},
		{
			name: "port too large",
			mutate: func(i *Input) {
				i.Port = 65536
			},
		},
		{
			name: "unsupported ssl mode",
			mutate: func(i *Input) {
				i.SSLMode = "invalid"
			},
		},
		{
			name: "password required for create",
			mutate: func(i *Input) {
				i.Password = ""
			},
		},
		{
			name: "password too long",
			mutate: func(i *Input) {
				i.Password = string(make([]byte, 1025))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validTestInput()
			tt.mutate(&input)

			if err := validate(input, true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validate() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestValidateUpdateCanOmitPassword(t *testing.T) {
	input := validTestInput()
	input.Password = ""

	if err := validate(input, false); err != nil {
		t.Fatalf("update should allow an omitted password, got %v", err)
	}
}

func TestEncryptDecryptPassword(t *testing.T) {
	repo := NewRepository(nil, "test-secret")
	password := "StrongPassword123!"

	encrypted, err := repo.encrypt(password)
	if err != nil {
		t.Fatalf("encrypt() error = %v", err)
	}
	if len(encrypted) == 0 {
		t.Fatal("encrypted password must not be empty")
	}
	if string(encrypted) == password {
		t.Fatal("encrypted password must not equal plaintext")
	}

	decrypted, err := repo.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if decrypted != password {
		t.Fatalf("decrypted password = %q, want original password", decrypted)
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	repo1 := NewRepository(nil, "secret-one")
	repo2 := NewRepository(nil, "secret-two")

	encrypted, err := repo1.encrypt("password")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo2.decrypt(encrypted); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestDecryptInvalidCiphertextFails(t *testing.T) {
	repo := NewRepository(nil, "test-secret")

	if _, err := repo.decrypt([]byte("invalid")); err == nil {
		t.Fatal("decrypt should reject invalid ciphertext")
	}
}
