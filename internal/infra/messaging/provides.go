package messaging

import (
	"github.com/google/wire"
	domainmessaging "github.com/torchwoodcloud/torchwood/internal/domain/messaging"
)

var ProviderSet = wire.NewSet(
	NewMailer,
	NewSMSService,
	wire.Bind(new(domainmessaging.Mailer), new(*MailerService)),
	wire.Bind(new(domainmessaging.SMSSender), new(*SMSService)),
)
