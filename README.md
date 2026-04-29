# Project Context

We at ScorePlay are committed to helping top sports organizations in the world better store, index, and distribute their content. Our goal is to enable them to focus on their core activities without spending hours searching for specific images. To achieve this, we need a robust application capable of indexing and tagging photos, making it easy to search for specific content.

# Project Description

You are required to develop an application. The application should be able to create media and tags through an HTTP API. The API should provide the following functionalities:

- **Create a Tag**: The application should allow the creation of tags. A tag can be anything like a player's name, location, specific game, or competition. The only mandatory data for tags is a **name**. You are free to structure the tag data as you see fit, as long as each tag has a name.
- **List All Tags**: The application should allow listing all tags.
- **Create a Media**: The application should allow the creation of media. For the purposes of this test, media will can be either photos or videos. Each media item should include at least a file, a **name**, and a **list of tags**. Your application must also handle the file upload process.
- **Retrieve a Media**: The application should allow retrieving a specific media by its ID.

You have the flexibility to design the API in any way you see fit, as long as it includes the mandatory features. 

Although it is not required for this project, consider the potential future need to search for media based on tags (e.g., finding all images tagged with a specific player's name). Please keep this in mind when making your design and architecture choices.

# Additional Considerations

- **Code Documentation**:
    - Provide clear and concise documentation for your API endpoints.
    - Include any necessary setup instructions to run your application locally.
- **Design and Technology Choices**:
    - Explain your design and technology choices in a markdown file or a separate document.
    - Discuss what you would improve if given more time.
    - Share any additional thoughts or considerations that went into your development process.
- **Testing**:
    - Ensure your application is thoroughly tested.
    - Include test cases and instructions on how to run them.
    

# **Requirements:**

- The application must be written in Golang.
- The application should be working and production-ready.
- Code quality should meet production-level standards.
- Include a markdown file or any separate document to explain your design an- d technology choices, what you would improve with more time, and any other things you want to share with us.

# **Evaluation Criteria:**

- **Functionality**: Does the application meet the requirements and provide the specified features?
- **Code Quality**: Is the code clean, readable, and maintainable?
- **Design and Architecture**: Are the design and technology choices well-explained and justified?
- **Documentation**: Is the documentation clear and helpful?
- **Testing**: Are there adequate tests, and do they cover the necessary cases?

---

### **Example API Specifications:**

While you are free to structure your API as you deem fit, here is an example specification to help you get started:

1. **Create a Tag**:
    - **Endpoint**: `POST /tags`
    - **Body**: `{ "name": "string" }`
    - **Response**: `201 Created`
2. **List All Tags**:
    - **Endpoint**: `GET /tags`
    - **Response**: `200 OK`
```jsx
{ "id": "string", "name": "string" } ]
```

3. **Create a Media**:
    - **Endpoint**: `POST /media`
    - **Body**: Form-data with fields `name` (string), `tags` (array of tag IDs), and `file` (binary)
    - **Response**: `201 Created`
4. **Retrieve a Media**:
    - **Endpoint**: `GET /media/{id}`
    - **Response**: `200 OK`
    
    ```jsx
    { "id": "string", "name": "string", "tags": [ "string" ], "fileUrl": "string" }
    ```
    

We look forward to seeing your innovative solutions and the thought process behind your implementation.