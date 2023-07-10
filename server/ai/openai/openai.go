package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/url"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/mattermost/mattermost-plugin-ai/server/ai"
	"github.com/pkg/errors"
	"github.com/sashabaranov/go-openai"
	openaiClient "github.com/sashabaranov/go-openai"
)

type OpenAI struct {
	client       *openaiClient.Client
	defaultModel string
}

const MaxFunctionCalls = 10

func NewCompatible(apiKey string, endpointUrl string, model string) *OpenAI {
	config := openai.DefaultConfig(apiKey)
	config.BaseURL = endpointUrl

	parsedUrl, err := url.Parse(endpointUrl)
	if err == nil && strings.HasSuffix(parsedUrl.Host, "openai.azure.com") {
		config = openai.DefaultAzureConfig(apiKey, endpointUrl)
	}
	return &OpenAI{
		client:       openaiClient.NewClientWithConfig(config),
		defaultModel: model,
	}
}

func New(apiKey string, defaultModel string) *OpenAI {
	if defaultModel == "" {
		defaultModel = openaiClient.GPT3Dot5Turbo
	}
	return &OpenAI{
		client:       openaiClient.NewClient(apiKey),
		defaultModel: defaultModel,
	}
}

func modifyCompletionRequestWithConversation(request openaiClient.ChatCompletionRequest, conversation ai.BotConversation) openaiClient.ChatCompletionRequest {
	request.Messages = postsToChatCompletionMessages(conversation.Posts)
	request.Functions = toolsToFunctionDefinitions(conversation.Tools)
	return request
}

func toolsToFunctionDefinitions(tools []ai.Tool) []openaiClient.FunctionDefinition {
	result := make([]openaiClient.FunctionDefinition, 0, len(tools))

	schemaMaker := jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
	}

	for _, tool := range tools {
		schema := schemaMaker.Reflect(tool.Schema)
		result = append(result, openaiClient.FunctionDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  schema,
		})
	}

	return result
}

func postsToChatCompletionMessages(posts []ai.Post) []openaiClient.ChatCompletionMessage {
	result := make([]openaiClient.ChatCompletionMessage, 0, len(posts))

	for _, post := range posts {
		role := openaiClient.ChatMessageRoleUser
		if post.Role == ai.PostRoleBot {
			role = openaiClient.ChatMessageRoleAssistant
		} else if post.Role == ai.PostRoleSystem {
			role = openaiClient.ChatMessageRoleSystem
		}
		result = append(result, openai.ChatCompletionMessage{
			Role:    role,
			Content: post.Message,
		})
	}

	return result
}

// createFunctionArrgmentResolver Creates a resolver for the json arguments of an openai function call. Unmarshaling the json into the supplied struct.
func createFunctionArrgmentResolver(jsonArgs string) ai.ArgumentGetter {
	return func(args any) error {
		return json.Unmarshal([]byte(jsonArgs), args)
	}
}

func handleStreamFunctionCall(request openaiClient.ChatCompletionRequest, name, arguments string) (openaiClient.ChatCompletionRequest, error) {
	toolResult, err := ai.ResolveTool(name, createFunctionArrgmentResolver(arguments))
	if err != nil {
		return request, err
	}
	request.Messages = append(request.Messages, openai.ChatCompletionMessage{
		Role:    openaiClient.ChatMessageRoleFunction,
		Name:    name,
		Content: toolResult,
	})

	return request, nil
}

func (s *OpenAI) streamResultToChannels(request openaiClient.ChatCompletionRequest, output chan<- string, errChan chan<- error) {
	request.Stream = true
	stream, err := s.client.CreateChatCompletionStream(context.Background(), request)
	if err != nil {
		errChan <- err
		return
	}

	defer stream.Close()

	// Buffering in the case of a function call.
	functionName := strings.Builder{}
	functionArguments := strings.Builder{}
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			errChan <- err
			return
		}

		// Check finishing conditions
		switch response.Choices[0].FinishReason {
		case "":
			// Not done yet, keep going
		case openaiClient.FinishReasonStop:
			return
		case openaiClient.FinishReasonFunctionCall:
			// Verify OpenAI functions are not recursing too deep.
			numFunctionCalls := 0
			for i := len(request.Messages) - 1; i >= 0; i-- {
				if request.Messages[i].Role == openaiClient.ChatMessageRoleFunction {
					numFunctionCalls++
				} else {
					break
				}
			}
			if numFunctionCalls > MaxFunctionCalls {
				errChan <- errors.New("Too many function calls")
				return
			}

			// Call ourselves again with the result of the function call
			recursiveRequest, err := handleStreamFunctionCall(request, functionName.String(), functionArguments.String())
			if err != nil {
				errChan <- err
				return
			}
			s.streamResultToChannels(recursiveRequest, output, errChan)
			return
		default:
			fmt.Printf("Unknown finish reason: %s", response.Choices[0].FinishReason)
			return
		}

		// Keep track of any function call received
		if response.Choices[0].Delta.FunctionCall != nil {
			if response.Choices[0].Delta.FunctionCall.Name != "" {
				functionName.WriteString(response.Choices[0].Delta.FunctionCall.Name)
			}
			if response.Choices[0].Delta.FunctionCall.Arguments != "" {
				functionArguments.WriteString(response.Choices[0].Delta.FunctionCall.Arguments)
			}
		}

		output <- response.Choices[0].Delta.Content
	}
}

func (s *OpenAI) streamResult(request openaiClient.ChatCompletionRequest) (*ai.TextStreamResult, error) {
	output := make(chan string)
	errChan := make(chan error)
	go func() {
		defer close(output)
		defer close(errChan)
		s.streamResultToChannels(request, output, errChan)
	}()

	return &ai.TextStreamResult{Stream: output, Err: errChan}, nil
}

func (s *OpenAI) GetDefaultConfig() ai.LLMConfig {
	return ai.LLMConfig{
		Model:     s.defaultModel,
		MaxTokens: 0,
	}
}

func (s *OpenAI) createConfig(opts []ai.LanguageModelOption) ai.LLMConfig {
	cfg := s.GetDefaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (s *OpenAI) completionReqeustFromConfig(cfg ai.LLMConfig) openaiClient.ChatCompletionRequest {
	return openaiClient.ChatCompletionRequest{
		Model:            cfg.Model,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      1.0,
		TopP:             1.0,
		FrequencyPenalty: 0,
		PresencePenalty:  0,
	}
}

func (s *OpenAI) ChatCompletion(conversation ai.BotConversation, opts ...ai.LanguageModelOption) (*ai.TextStreamResult, error) {
	request := s.completionReqeustFromConfig(s.createConfig(opts))
	request = modifyCompletionRequestWithConversation(request, conversation)
	request.Stream = true
	return s.streamResult(request)
}

func (s *OpenAI) ChatCompletionNoStream(conversation ai.BotConversation, opts ...ai.LanguageModelOption) (string, error) {
	request := s.completionReqeustFromConfig(s.createConfig(opts))
	request = modifyCompletionRequestWithConversation(request, conversation)
	response, err := s.client.CreateChatCompletion(context.Background(), request)
	if err != nil {
		return "", err
	}
	return response.Choices[0].Message.Content, nil
}

func (s *OpenAI) Transcribe(file io.Reader) (string, error) {
	resp, err := s.client.CreateTranscription(context.Background(), openaiClient.AudioRequest{
		Model:    openaiClient.Whisper1,
		Reader:   file,
		FilePath: "input.mp3",
		Format:   openaiClient.AudioResponseFormatJSON,
	})
	if err != nil {
		return "", errors.Wrap(err, "unable to create whisper transcription")
	}

	return resp.Text, nil
}

func (s *OpenAI) GenerateImage(prompt string) (image.Image, error) {
	req := openaiClient.ImageRequest{
		Prompt:         prompt,
		Size:           openai.CreateImageSize256x256,
		ResponseFormat: openai.CreateImageResponseFormatB64JSON,
		N:              1,
	}

	respBase64, err := s.client.CreateImage(context.Background(), req)
	if err != nil {
		return nil, err
	}

	imgBytes, err := base64.StdEncoding.DecodeString(respBase64.Data[0].B64JSON)
	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(imgBytes)
	imgData, err := png.Decode(r)
	if err != nil {
		return nil, err
	}

	return imgData, nil
}
