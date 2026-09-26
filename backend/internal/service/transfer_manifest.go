package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/constants"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/dto"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/model"
	"github.com/blueship581/hazardous-waste-transfer-compliance/backend/internal/repository"
	"gorm.io/gorm"
)

type TransferManifestService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.TransferManifest], error)
	Get(context.Context, uint) (model.TransferManifest, error)
	Create(context.Context, dto.CreateTransferManifest, string, string) (model.TransferManifest, error)
	Update(context.Context, uint, dto.UpdateTransferManifest, string, string) (model.TransferManifest, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string) (model.TransferManifest, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type transferManifestService struct {
	repository repository.TransferManifestRepository
	generators repository.WasteGeneratorRepository
	carriers   repository.CarrierProfileRepository
	checks     repository.ComplianceCheckRepository
	security   SecurityService
}

func NewTransferManifestService(repo repository.TransferManifestRepository, generators repository.WasteGeneratorRepository, carriers repository.CarrierProfileRepository, checks repository.ComplianceCheckRepository, security SecurityService) TransferManifestService {
	return &transferManifestService{repository: repo, generators: generators, carriers: carriers, checks: checks, security: security}
}

func (s *transferManifestService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.TransferManifest], error) {
	return s.repository.List(ctx, query)
}

func (s *transferManifestService) Get(ctx context.Context, id uint) (model.TransferManifest, error) {
	return s.repository.Get(ctx, id)
}

func (s *transferManifestService) Create(ctx context.Context, input dto.CreateTransferManifest, actor, requestID string) (model.TransferManifest, error) {
	if err := validateTransferManifestBusinessFields(input.Code, input.Name, input.Facility, input.Owner, input.GeneratorCode, input.CarrierCode, input.WasteCode, input.Destination, input.Evidence, input.QuantityKg); err != nil {
		return model.TransferManifest{}, err
	}
	if _, err := s.generators.FindByCode(ctx, input.GeneratorCode); err != nil {
		return model.TransferManifest{}, fmt.Errorf("%w: generator %s does not exist", ErrInvalidInput, input.GeneratorCode)
	}
	if _, err := s.carriers.FindByCode(ctx, input.CarrierCode); err != nil {
		return model.TransferManifest{}, fmt.Errorf("%w: carrier %s does not exist", ErrInvalidInput, input.CarrierCode)
	}
	item := model.TransferManifest{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.TransferManifestInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		GeneratorCode: strings.ToUpper(strings.TrimSpace(input.GeneratorCode)), CarrierCode: strings.ToUpper(strings.TrimSpace(input.CarrierCode)),
		WasteCode: strings.ToUpper(strings.TrimSpace(input.WasteCode)), QuantityKg: input.QuantityKg, Destination: strings.TrimSpace(input.Destination),
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
	}
	if err := s.repository.CreateAudited(ctx, &item, newAuditLog(actor, requestID, "create", "TransferManifest", "", item.Status, "created linked transfer manifest")); err != nil {
		return model.TransferManifest{}, fmt.Errorf("create 转运清单: %w", err)
	}
	return item, nil
}

func (s *transferManifestService) Update(ctx context.Context, id uint, input dto.UpdateTransferManifest, actor, requestID string) (model.TransferManifest, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.TransferManifest{}, err
	}
	if current.Status != "draft" {
		return model.TransferManifest{}, fmt.Errorf("%w: only draft manifests can be edited", ErrInvalidInput)
	}
	if err := validateTransferManifestBusinessFields(current.Code, input.Name, input.Facility, input.Owner, input.GeneratorCode, input.CarrierCode, input.WasteCode, input.Destination, input.Evidence, input.QuantityKg); err != nil {
		return model.TransferManifest{}, err
	}
	if _, err := s.generators.FindByCode(ctx, input.GeneratorCode); err != nil {
		return model.TransferManifest{}, fmt.Errorf("%w: generator %s does not exist", ErrInvalidInput, input.GeneratorCode)
	}
	if _, err := s.carriers.FindByCode(ctx, input.CarrierCode); err != nil {
		return model.TransferManifest{}, fmt.Errorf("%w: carrier %s does not exist", ErrInvalidInput, input.CarrierCode)
	}
	current.Name = strings.TrimSpace(input.Name)
	current.GeneratorCode = strings.ToUpper(strings.TrimSpace(input.GeneratorCode))
	current.CarrierCode = strings.ToUpper(strings.TrimSpace(input.CarrierCode))
	current.WasteCode = strings.ToUpper(strings.TrimSpace(input.WasteCode))
	current.QuantityKg = input.QuantityKg
	current.Destination = strings.TrimSpace(input.Destination)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateAudited(ctx, id, input.ExpectedVersion, &current, newAuditLog(actor, requestID, "update", "TransferManifest", current.Status, current.Status, "updated draft manifest and evidence")); err != nil {
		return model.TransferManifest{}, fmt.Errorf("update 转运清单: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *transferManifestService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.TransferManifest, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.TransferManifest{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.TransferManifestTransitions, current.Status, target) {
		return model.TransferManifest{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	if input.ExpectedVersion != current.Version {
		return model.TransferManifest{}, fmt.Errorf("%w: expected v%d, record is v%d", repository.ErrVersionConflict, input.ExpectedVersion, current.Version)
	}
	if target == "submitted" || target == "in_transit" {
		if err := s.validateLinkedParties(ctx, current); err != nil {
			return model.TransferManifest{}, err
		}
	}
	authorizingCheck := ""
	if target == "in_transit" {
		check, err := s.evaluateShipmentGate(ctx, current, actor, requestID)
		if err != nil {
			return model.TransferManifest{}, err
		}
		authorizingCheck = fmt.Sprintf("%s v%d", check.Code, check.Version)
	}
	before := current.Status
	current.Status = target
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	detail := input.Reason
	if authorizingCheck != "" {
		detail = fmt.Sprintf("%s；放行依据：核验 %s 对清单 v%d 的通过决定", strings.TrimSpace(detail), authorizingCheck, current.Version-1)
	}
	if err := s.repository.UpdateAudited(ctx, id, input.ExpectedVersion, &current, newAuditLog(actor, requestID, "transition", "TransferManifest", before, target, detail)); err != nil {
		return model.TransferManifest{}, fmt.Errorf("transition 转运清单: %w", err)
	}
	return s.repository.Get(ctx, id)
}

// evaluateShipmentGate aligns dispatch with verification: only the latest check
// recorded against the same manifest code AND the manifest version currently
// being shipped may release the load, and only when its decision is "pass".
// Missing records, version drift, pending work, failures and escalations block
// the shipment; every block is audited under the request ID so operators see
// which check number held the load and why.
func (s *transferManifestService) evaluateShipmentGate(ctx context.Context, manifest model.TransferManifest, actor, requestID string) (model.ComplianceCheck, error) {
	latest, err := s.checks.LatestByManifestCode(ctx, manifest.Code)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, model.ComplianceCheck{},
				fmt.Sprintf("清单 %s 暂无任何合规核验记录，禁止发运", manifest.Code))
		}
		return model.ComplianceCheck{}, fmt.Errorf("query compliance gate: %w", err)
	}
	if latest.ManifestVersion != manifest.Version {
		return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, latest,
			fmt.Sprintf("清单 %s 当前为 v%d，但最新核验 %s 绑定的是 v%d，核验版本与清单不一致，禁止发运",
				manifest.Code, manifest.Version, latest.Code, latest.ManifestVersion))
	}
	switch latest.Status {
	case string(constants.CheckStatePending):
		return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, latest,
			fmt.Sprintf("核验 %s 对清单 %s v%d 仍待处理，等待复核决定期间禁止发运",
				latest.Code, manifest.Code, manifest.Version))
	case string(constants.CheckStateFail):
		return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, latest,
			fmt.Sprintf("核验 %s 对清单 %s v%d 的决定为不通过（%s），禁止发运",
				latest.Code, manifest.Code, manifest.Version, gateReason(latest)))
	case string(constants.CheckStateEscalated):
		return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, latest,
			fmt.Sprintf("核验 %s 对清单 %s v%d 已升级复核，复核结论形成前禁止发运（%s）",
				latest.Code, manifest.Code, manifest.Version, gateReason(latest)))
	case string(constants.CheckStatePass):
		return latest, nil
	default:
		return model.ComplianceCheck{}, s.blockShipment(ctx, manifest, actor, requestID, latest,
			fmt.Sprintf("核验 %s 状态 %s 不是有效放行决定，禁止发运", latest.Code, latest.Status))
	}
}

func gateReason(check model.ComplianceCheck) string {
	if reason := strings.TrimSpace(check.DecisionBasis); reason != "" {
		return reason
	}
	return "未记录决定依据"
}

func (s *transferManifestService) blockShipment(ctx context.Context, manifest model.TransferManifest, actor, requestID string, check model.ComplianceCheck, reason string) error {
	detail := fmt.Sprintf("发运拦截：%s", reason)
	if check.ID != 0 {
		detail = fmt.Sprintf("%s（核验编号：%s）", detail, check.Code)
	}
	if err := s.security.Audit(ctx, actor, requestID, "shipment_blocked", "TransferManifest", manifest.ID,
		manifest.Status, manifest.Status, detail); err != nil {
		return fmt.Errorf("persist shipment block audit: %w", err)
	}
	return fmt.Errorf("%w: %s", ErrShipmentBlocked, reason)
}

func (s *transferManifestService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != "draft" {
		return fmt.Errorf("%w: submitted manifests must be retained for compliance", ErrInvalidInput)
	}
	return s.repository.DeleteAudited(ctx, id, newAuditLog(actor, requestID, "delete", "TransferManifest", current.Status, "deleted", "soft deleted draft manifest"))
}

func (s *transferManifestService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func (s *transferManifestService) validateLinkedParties(ctx context.Context, manifest model.TransferManifest) error {
	generator, err := s.generators.FindByCode(ctx, manifest.GeneratorCode)
	if err != nil {
		return fmt.Errorf("%w: linked generator is unavailable", ErrInvalidInput)
	}
	if generator.Status != "active" || !generator.PermitExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("%w: generator permit must be active and unexpired", ErrInvalidInput)
	}
	carrier, err := s.carriers.FindByCode(ctx, manifest.CarrierCode)
	if err != nil {
		return fmt.Errorf("%w: linked carrier is unavailable", ErrInvalidInput)
	}
	if carrier.Status != "verified" || !carrier.LicenseExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("%w: carrier license must be verified and unexpired", ErrInvalidInput)
	}
	return nil
}

func validateTransferManifestBusinessFields(code, name, facility, owner, generatorCode, carrierCode, wasteCode, destination, evidence string, quantityKg float64) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(generatorCode) == "" || strings.TrimSpace(carrierCode) == "" || strings.TrimSpace(wasteCode) == "" || strings.TrimSpace(destination) == "" {
		return fmt.Errorf("%w: manifest identity, parties and route are required", ErrInvalidInput)
	}
	if quantityKg <= 0 || strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("%w: positive waste quantity and manifest evidence are required", ErrInvalidInput)
	}
	return nil
}
